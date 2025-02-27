package tracing

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/cespare/xxhash/v2"
	ua "github.com/mileusna/useragent"
	"github.com/segmentio/encoding/json"
	"github.com/uptrace/uptrace/pkg/attrkey"
	"github.com/uptrace/uptrace/pkg/bunapp"
	"github.com/uptrace/uptrace/pkg/bunotel"
	"github.com/uptrace/uptrace/pkg/bunutil"
	"github.com/uptrace/uptrace/pkg/logparser"
	"github.com/uptrace/uptrace/pkg/org"
	"github.com/uptrace/uptrace/pkg/otlpconv"
	"github.com/uptrace/uptrace/pkg/sqlparser"
	"github.com/uptrace/uptrace/pkg/tracing/anyconv"
	"github.com/uptrace/uptrace/pkg/tracing/norm"
	"github.com/uptrace/uptrace/pkg/unsafeconv"
	"github.com/uptrace/uptrace/pkg/utf8util"
	"github.com/uptrace/uptrace/pkg/uuid"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func initSpanOrEvent(ctx *spanContext, app *bunapp.App, span *Span) {
	project, ok := ctx.Project(app, span.ProjectID)
	if !ok {
		return
	}

	processAttrs(ctx, span)

	if span.EventName != "" {
		assignEventSystemAndGroupID(ctx, project, span)
		span.EventName = utf8util.TruncLC(span.EventName)
	} else {
		assignSpanSystemAndGroupID(ctx, project, span)
		span.Name = utf8util.TruncLC(span.Name)
	}

	if name, _ := span.Attrs[attrkey.DisplayName].(string); name != "" {
		span.DisplayName = utf8util.TruncSmall(name)
		delete(span.Attrs, attrkey.DisplayName)
	} else if span.DisplayName != "" {
		span.DisplayName = utf8util.TruncSmall(span.DisplayName)
	} else {
		span.DisplayName = span.EventOrSpanName()
	}

	span.System = utf8util.TruncSmall(span.System)
}

func processAttrs(ctx *spanContext, span *Span) {
	normAttrs(span.Attrs, span.Attrs)

	if msg, _ := span.Attrs[attrkey.LogMessage].(string); msg != "" {
		parseLogMessage(ctx, span, msg)
	}
	if s, _ := span.Attrs[attrkey.HTTPUserAgent].(string); s != "" {
		initHTTPUserAgent(span.Attrs, s)
	}

	if span.TraceID.IsZero() {
		// Standalone span.
		span.TraceID = uuid.Rand()
		span.ID = 0
		span.ParentID = 0
		span.Standalone = true
	}
	if !span.Standalone {
		if span.ID == 0 {
			span.ID = rand.Uint64()
		}
	}
	if span.Time.IsZero() {
		span.Time = time.Now()
	}

	if span.EventName == otelEventLog {
		if _, ok := span.Attrs[attrkey.LogSeverity]; !ok {
			span.Attrs[attrkey.LogSeverity] = bunotel.InfoSeverity
		}
	}
}

func initHTTPUserAgent(attrs AttrMap, str string) {
	agent := ua.Parse(str)

	if agent.Name != "" {
		attrs[attrkey.HTTPUserAgentName] = agent.Name
	}
	if agent.Version != "" {
		attrs[attrkey.HTTPUserAgentVersion] = agent.Version
	}

	if agent.OS != "" {
		attrs[attrkey.HTTPUserAgentOS] = agent.OS
	}
	if agent.OSVersion != "" {
		attrs[attrkey.HTTPUserAgentOSVersion] = agent.OSVersion
	}

	if agent.Device != "" {
		attrs[attrkey.HTTPUserAgentDevice] = agent.Device
	}

	if agent.Bot {
		attrs[attrkey.HTTPUserAgentBot] = 1
	}
}

//------------------------------------------------------------------------------

func parseLogMessage(ctx *spanContext, span *Span, msg string) {
	if params, ok := logparser.IsJSON(msg); ok {
		parseJSONLogMessage(ctx, span, params)
	} else {
		parseTextLogMessage(ctx, span, msg)
	}
}

func parseJSONLogMessage(ctx *spanContext, span *Span, params AttrMap) {
	msg := popLogMessageParam(params)
	populateSpanFromParams(span, params)

	if msg != "" {
		span.Attrs[attrkey.LogMessage] = msg
		parseTextLogMessage(ctx, span, msg)
	}
}

func parseTextLogMessage(ctx *spanContext, span *Span, msg string) {
	hash, params := messageHashAndParams(ctx, msg)
	if span.EventName == otelEventLog {
		span.logMessageHash = hash
	}
	populateSpanFromParams(span, params)
}

func popLogMessageParam(params AttrMap) string {
	for _, key := range []string{"log", "message", "msg"} {
		if value, _ := params[key].(string); value != "" {
			delete(params, key)
			return value
		}
	}
	return ""
}

func populateSpanFromParams(span *Span, params AttrMap) {
	attrs := span.Attrs
	flattenAttrValues(params)
	normAttrs(span.Attrs, params)

	if span.Time.IsZero() {
		for _, key := range []string{"timestamp", "datetime", "time"} {
			value, _ := params[key].(string)
			if value == "" {
				continue
			}

			tm := anyconv.Time(value)
			if tm.IsZero() {
				continue
			}

			span.Time = tm
			delete(params, key)
			break
		}
	}

	if span.TraceID.IsZero() {
		for _, key := range []string{"trace_id", "traceid"} {
			value, _ := params[key].(string)
			if value == "" {
				continue
			}

			traceID, err := uuid.Parse(value)
			if err != nil {
				continue
			}

			if !traceID.IsZero() {
				span.TraceID = traceID
				delete(params, key)
				break
			}
		}
	}

	if span.ParentID == 0 {
		for _, key := range []string{"span_id", "spanid"} {
			value, _ := params[key].(string)
			if value == "" {
				continue
			}

			spanID, err := parseSpanID(value)
			if err != nil {
				continue
			}

			if spanID != 0 {
				span.ParentID = spanID
				delete(params, key)
				break
			}
		}
	}

	for key, value := range params {
		attrs.SetClashingKeys(key, value)
	}
}

func normAttrs(attrs, params AttrMap) {
	if _, ok := attrs[attrkey.ServiceName]; !ok {
		for _, key := range []string{
			attrkey.ServiceName,
			"service_name",
			"service",
		} {
			value, _ := params[key].(string)
			if value == "" {
				continue
			}
			attrs[attrkey.ServiceName] = value
			delete(params, key)
			break
		}
	}

	if _, ok := attrs[attrkey.HTTPRoute]; !ok {
		for _, key := range []string{
			attrkey.HTTPRoute,
			"http_route",
		} {
			value, _ := params[key].(string)
			if value == "" {
				continue
			}
			attrs[attrkey.HTTPRoute] = value
			delete(params, key)
			break
		}
	}

	if _, ok := attrs[attrkey.DBSystem]; !ok {
		for _, key := range []string{
			attrkey.DBSystem,
			"db_system",
			"db.type",
		} {
			value, _ := params[key].(string)
			if value == "" {
				continue
			}
			attrs[attrkey.DBSystem] = value
			delete(params, key)
			break
		}
	}

	if _, ok := attrs[attrkey.DBName]; !ok {
		for _, key := range []string{
			attrkey.DBName,
			"db_name",
			"dbname",
		} {
			value, _ := params[key].(string)
			if value == "" {
				continue
			}
			attrs[attrkey.DBSystem] = value
			delete(params, key)
			break
		}
	}

	if _, ok := attrs[attrkey.DBStatement]; !ok {
		for _, key := range []string{
			attrkey.DBStatement,
			"db_statement",
			"statement",
		} {
			value, _ := params[key].(string)
			if value == "" {
				continue
			}
			attrs[attrkey.DBStatement] = value
			delete(params, key)
			break
		}
	}

	if _, ok := attrs[attrkey.LogSeverity]; !ok {
		for _, key := range []string{
			attrkey.LogSeverity,
			"log_severity",
			"severity",
			"error_severity",
			"log.level",
			"level",
		} {
			severity, _ := params[key].(string)
			if severity == "" {
				continue
			}

			if severity := norm.LogSeverity(severity); severity != "" {
				attrs[attrkey.LogSeverity] = severity
				delete(params, key)
				break
			}
		}
	}
}

func flattenAttrValues(attrs AttrMap) {
loop:
	for key, value := range attrs {
		switch key {
		case attrkey.LogMessage, attrkey.ExceptionMessage:
			// Keep log and exception messages as is.
			continue loop
		}

		switch value := value.(type) {
		case nil:
			delete(attrs, key)
			continue loop
		case map[string]any:
			delete(attrs, key)
			attrs.Flatten(value, key+".")
			continue loop
		case string:
			if value, ok := bunutil.IsJSON(value); ok {
				delete(attrs, key)
				attrs.Flatten(value, key+".")
				continue loop
			}
		}
	}
}

func messageHashAndParams(
	ctx *spanContext, msg string,
) (uint64, map[string]any) {
	digest := ctx.digest
	digest.Reset()

	var params map[string]any

	tok := logparser.NewTokenizer(msg)
loop:
	for {
		tok := tok.NextToken()
		switch tok.Type {
		case logparser.InvalidToken:
			break loop
		case logparser.WordToken:
			digest.WriteString(tok.Text)
		case logparser.ParamToken:
			if k, v, ok := logparser.IsLogfmt(tok.Text); ok {
				if params == nil {
					params = make(map[string]any)
				}
				params[k] = v
			}
		}
	}

	return digest.Sum64(), params
}

//------------------------------------------------------------------------------

func newSpanLink(link *tracepb.Span_Link) *SpanLink {
	return &SpanLink{
		TraceID: otlpTraceID(link.TraceId),
		SpanID:  otlpSpanID(link.SpanId),
		Attrs:   otlpconv.Map(link.Attributes),
	}
}

func hashDBStmt(digest *xxhash.Digest, s string) uint64 {
	tok := sqlparser.NewTokenizer(s)
	for {
		token, err := tok.NextToken()
		if err == io.EOF {
			break
		}
		if token.Type == sqlparser.IdentToken && isSQLKeyword(token.Text) {
			digest.WriteString(token.Text)
		}
	}
	return digest.Sum64()
}

func isSQLKeyword(s string) bool {
	switch strings.ToUpper(s) {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "TRUNCATE",
		"WITH", "FROM", "TABLE", "JOIN", "UNION", "WHERE", "GROUP", "LIMIT", "ORDER", "HAVING":
		return true
	default:
		return false
	}
}

func assignSpanSystemAndGroupID(ctx *spanContext, project *org.Project, span *Span) {
	if s := span.Attrs.Text(attrkey.RPCSystem); s != "" {
		span.Type = SpanTypeRPC
		span.System = SpanTypeRPC + ":" + span.Attrs.ServiceNameOrUnknown()
		span.GroupID = spanHash(ctx.digest, func(digest *xxhash.Digest) {
			hashSpan(project, digest, span,
				attrkey.RPCSystem,
				attrkey.RPCService,
				attrkey.RPCMethod,
			)
		})
		return
	}

	if s := span.Attrs.Text(attrkey.MessagingSystem); s != "" {
		span.Type = SpanTypeMessaging
		span.System = SpanTypeMessaging + ":" + s
		span.GroupID = spanHash(ctx.digest, func(digest *xxhash.Digest) {
			hashSpan(project, digest, span,
				attrkey.MessagingSystem,
				attrkey.MessagingOperation,
				attrkey.MessagingDestinationName,
				attrkey.MessagingDestinationKind,
			)
		})
		return
	}

	if dbSystem := span.Attrs.Text(attrkey.DBSystem); dbSystem != "" ||
		span.Attrs.Has(attrkey.DBStatement) {
		if dbSystem == "" {
			dbSystem = "unknown_db"
		}

		span.Type = SpanTypeDB
		span.System = SpanTypeDB + ":" + dbSystem
		stmt, _ := span.Attrs[attrkey.DBStatement].(string)

		span.GroupID = spanHash(ctx.digest, func(digest *xxhash.Digest) {
			hashSpan(project, digest, span, attrkey.DBName, attrkey.DBOperation, attrkey.DBSqlTable)
			if stmt != "" {
				hashDBStmt(digest, stmt)
			}
		})
		if stmt != "" {
			span.DisplayName = stmt
		}
		return
	}

	if span.Attrs.Has(attrkey.HTTPRoute) || span.Attrs.Has(attrkey.HTTPTarget) {
		span.Type = SpanTypeHTTP
		span.System = SpanTypeHTTP + ":" + span.Attrs.ServiceNameOrUnknown()
		span.GroupID = spanHash(ctx.digest, func(digest *xxhash.Digest) {
			hashSpan(project, digest, span, attrkey.HTTPMethod, attrkey.HTTPRoute)
		})
		return
	}

	if project.GroupFuncsByService &&
		(span.ParentID == 0 ||
			span.Kind != InternalSpanKind ||
			span.Attrs.Has(attrkey.CodeFunction)) {
		span.Type = SpanTypeFuncs
		span.System = SpanTypeFuncs + ":" + span.Attrs.ServiceNameOrUnknown()
		span.GroupID = spanHash(ctx.digest, func(digest *xxhash.Digest) {
			hashSpan(project, digest, span)
		})
		return
	}

	span.Type = SpanTypeFuncs
	span.System = SpanTypeFuncs
	span.GroupID = spanHash(ctx.digest, func(digest *xxhash.Digest) {
		hashSpan(project, digest, span)
	})
}

func spanHash(digest *xxhash.Digest, fn func(digest *xxhash.Digest)) uint64 {
	digest.Reset()
	fn(digest)
	return digest.Sum64()
}

func hashSpan(project *org.Project, digest *xxhash.Digest, span *Span, keys ...string) {
	if project.GroupByEnv {
		if env := span.Attrs.Text(attrkey.DeploymentEnvironment); env != "" {
			digest.WriteString(env)
		}
	}
	digest.WriteString(span.System)
	digest.WriteString(span.Kind)
	if span.EventName != "" {
		digest.WriteString(span.EventName)
	} else {
		digest.WriteString(span.Name)
	}

	for _, key := range keys {
		if value, ok := span.Attrs[key]; ok {
			digest.WriteString(key)
			digest.WriteString(fmt.Sprint(value))
		}
	}
}

//------------------------------------------------------------------------------

func initEventFromHostSpan(dest *Span, event *SpanEvent, hostSpan *Span) {
	dest.EventName = event.Name
	dest.Time = event.Time
	dest.Attrs = event.Attrs

	dest.ProjectID = hostSpan.ProjectID
	dest.TraceID = hostSpan.TraceID
	dest.ID = rand.Uint64()
	dest.ParentID = hostSpan.ID

	dest.Name = hostSpan.Name
	dest.Kind = hostSpan.Kind
	for k, v := range hostSpan.Attrs {
		if _, ok := dest.Attrs[k]; !ok {
			dest.Attrs[k] = v
		}
	}
	dest.StatusCode = hostSpan.StatusCode
}

func initEvent(ctx *spanContext, app *bunapp.App, span *Span) {
	project, ok := ctx.Project(app, span.ProjectID)
	if !ok {
		return
	}

	processAttrs(ctx, span)
	assignEventSystemAndGroupID(ctx, project, span)
}

func assignEventSystemAndGroupID(ctx *spanContext, project *org.Project, span *Span) {
	if span.EventName == otelEventError {
		span.EventName = otelEventException
	}

	switch span.EventName {
	case otelEventLog:
		handleLogEvent(ctx, project, span)
		return
	case otelEventException:
		handleExceptionEvent(ctx, project, span)
		return
	case otelEventMessage:
		system := eventMessageSystem(span)
		span.Type = EventTypeMessage
		span.System = EventTypeMessage + ":" + system
		span.GroupID = spanHash(ctx.digest, func(digest *xxhash.Digest) {
			hashSpan(project, digest, span,
				attrkey.RPCSystem,
				attrkey.RPCService,
				attrkey.RPCMethod,
				attrkey.MessageType,
			)
		})
		span.DisplayName = eventMessageDisplayName(span)
		return
	}

	if span.Attrs.Has(attrkey.LogMessage) {
		handleLogEvent(ctx, project, span)
		return
	}

	if span.Attrs.Has(attrkey.ExceptionMessage) {
		handleExceptionEvent(ctx, project, span)
		return
	}

	span.Type = EventTypeOther
	span.System = EventTypeOther
	span.GroupID = spanHash(ctx.digest, func(digest *xxhash.Digest) {
		hashSpan(project, digest, span)
	})
}

func handleLogEvent(ctx *spanContext, project *org.Project, span *Span) {
	sev, _ := span.Attrs[attrkey.LogSeverity].(string)
	if sev == "" {
		sev = bunotel.InfoSeverity
	}

	span.Type = EventTypeLog
	span.System = EventTypeLog + ":" + strings.ToLower(sev)
	span.GroupID = spanHash(ctx.digest, func(digest *xxhash.Digest) {
		hashSpan(project, digest, span,
			attrkey.LogSeverity,
		)
		if span.logMessageHash != 0 {
			digest.WriteString(strconv.FormatUint(span.logMessageHash, 10))
		}
	})
	span.DisplayName = logDisplayName(span)
}

func handleExceptionEvent(ctx *spanContext, project *org.Project, span *Span) {
	span.Type = EventTypeLog
	span.System = SystemLogError
	span.GroupID = spanHash(ctx.digest, func(digest *xxhash.Digest) {
		hashSpan(project, digest, span, attrkey.ExceptionType)
		if s, _ := span.Attrs[attrkey.ExceptionMessage].(string); s != "" {
			hashMessage(ctx.digest, s)
		}
	})
	span.DisplayName = joinTypeMessage(
		span.Attrs.Text(attrkey.ExceptionType),
		span.Attrs.Text(attrkey.ExceptionMessage),
	)
}

func logDisplayName(span *Span) string {
	if msg, _ := span.Attrs[attrkey.LogMessage].(string); msg != "" {
		sev, _ := span.Attrs[attrkey.LogSeverity].(string)
		if sev != "" && !strings.HasPrefix(msg, sev) {
			msg = sev + " " + msg
		}
		return msg
	}

	if name := exceptionDisplayName(span); name != span.EventName {
		return name
	}

	if b, err := json.Marshal(span.Attrs); err == nil {
		return unsafeconv.String(b)
	}

	return span.EventName
}

func exceptionDisplayName(span *Span) string {
	typ, _ := span.Attrs[attrkey.ExceptionType].(string)
	msg, _ := span.Attrs[attrkey.ExceptionMessage].(string)
	if typ != "" || msg != "" {
		return joinTypeMessage(typ, msg)
	}
	return span.EventName
}

func eventMessageSystem(span *Span) string {
	if sys := span.Attrs.Text(attrkey.RPCSystem); sys != "" {
		return sys
	}
	if sys := span.Attrs.Text(attrkey.MessagingSystem); sys != "" {
		return sys
	}
	return SystemUnknown
}

func eventMessageDisplayName(span *Span) string {
	if span.EventName != otelEventMessage {
		return span.EventName
	}
	if op := span.Attrs.Text(attrkey.MessagingOperation); op != "" {
		return join(span.Name, op)
	}
	if typ := span.Attrs.Text("message.type"); typ != "" {
		return join(span.Name, typ)
	}
	if span.Kind != InternalSpanKind {
		return join(span.Name, span.Kind)
	}
	return span.EventName
}

func hashMessage(digest *xxhash.Digest, msg string) {
	tok := logparser.NewTokenizer(msg)
loop:
	for {
		tok := tok.NextToken()
		switch tok.Type {
		case logparser.InvalidToken:
			break loop
		case logparser.WordToken:
			digest.WriteString(tok.Text)
		}
	}
}

func joinTypeMessage(typ, msg string) string {
	if msg == "" {
		if typ == "" {
			return ""
		}
		return typ
	}
	if strings.HasPrefix(msg, typ) {
		return msg
	}
	return typ + ": " + msg
}

func join(s1, s2 string) string {
	if s1 != "" {
		return s1 + " " + s2
	}
	return s2
}

func parseSpanID(s string) (uint64, error) {
	if len(s) == 16 {
		if b, err := hex.DecodeString(s); err == nil {
			return binary.BigEndian.Uint64(b), nil
		}
	}
	return strconv.ParseUint(s, 10, 64)
}
