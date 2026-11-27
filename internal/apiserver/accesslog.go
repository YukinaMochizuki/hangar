package apiserver

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// accessLogMessage is the message of every access line, which the host's
// log pipeline selects them by (docs/decisions/monitoring.md).
const accessLogMessage = "rpc"

// maxLoggedName bounds the project or tenant an access line names: a
// caller refused for its credentials may send any string.
const maxLoggedName = 64

// accessLog writes one line for each Connect call a listener serves, as the
// call ends: the procedure, its code, how long it took, who called and on
// which project and tenant. It goes before the authenticating interceptor,
// so calls refused for their credentials are logged too, and learns the
// principal from its principal interceptor, which goes after it.
type accessLog struct {
	s *Server
	// tenants maps each project seen to its tenant. A project's name is never
	// given to another, so an entry never goes stale.
	tenants sync.Map
}

type accessKey struct{}

// accessCall is what the interceptors after accessLog tell it of a call.
type accessCall struct {
	procedure string
	principal atomic.Pointer[auth.Principal]
	project   atomic.Pointer[string]
	// tenant is the one the request names itself, as TenantService's do.
	tenant atomic.Pointer[string]
}

func (a *accessLog) begin(ctx context.Context, procedure string) (context.Context, *accessCall) {
	c := &accessCall{procedure: procedure}
	return context.WithValue(ctx, accessKey{}, c), c
}

func (a *accessLog) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		start := time.Now()
		ctx, c := a.begin(ctx, req.Spec().Procedure)
		if m, ok := req.Any().(proto.Message); ok {
			c.noteRequest(m)
		}
		resp, err := next(ctx, req)
		a.write(ctx, c, req.Spec().Procedure, false, time.Since(start), err)
		return resp, err
	}
}

func (a *accessLog) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *accessLog) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		ctx, c := a.begin(ctx, conn.Spec().Procedure)
		err := next(ctx, &accessConn{StreamingHandlerConn: conn, call: c})
		a.write(ctx, c, conn.Spec().Procedure, true, time.Since(start), err)
		return err
	}
}

// accessConn takes what a stream is about from the first message it
// receives.
type accessConn struct {
	connect.StreamingHandlerConn
	call *accessCall
	seen atomic.Bool
}

func (c *accessConn) Receive(msg any) error {
	err := c.StreamingHandlerConn.Receive(msg)
	if m, ok := msg.(proto.Message); ok && err == nil && !c.seen.Swap(true) {
		c.call.noteRequest(m)
	}
	return err
}

// principal is the interceptor that tells accessLog who a call was
// authenticated as. It goes after the authenticating interceptor.
func (a *accessLog) principal() connect.Interceptor { return accessPrincipal{} }

type accessPrincipal struct{}

func notePrincipal(ctx context.Context) {
	c, ok := ctx.Value(accessKey{}).(*accessCall)
	if !ok {
		return
	}
	if p, ok := auth.FromContext(ctx); ok {
		c.principal.Store(&p)
	}
}

func (accessPrincipal) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if !req.Spec().IsClient {
			notePrincipal(ctx)
		}
		return next(ctx, req)
	}
}

func (accessPrincipal) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (accessPrincipal) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		notePrincipal(ctx)
		return next(ctx, conn)
	}
}

// The services whose objects are projects and tenants themselves: their
// requests name the project or tenant as the object's name.
var (
	projectServicePrefix = "/" + tenancyv1alpha1connect.ProjectServiceName + "/"
	tenantServicePrefix  = "/" + tenancyv1alpha1connect.TenantServiceName + "/"
)

// noteRequest records the project and the tenant m names.
func (c *accessCall) noteRequest(m proto.Message) {
	var project, tenant string
	switch {
	case strings.HasPrefix(c.procedure, projectServicePrefix):
		project = nameOf(m)
	case strings.HasPrefix(c.procedure, tenantServicePrefix):
		tenant = nameOf(m)
	default:
		project = projectOf(m)
		if r, ok := m.(interface{ GetTenant() string }); ok {
			tenant = r.GetTenant()
		}
	}
	storeBounded(&c.project, project)
	storeBounded(&c.tenant, tenant)
}

func storeBounded(p *atomic.Pointer[string], s string) {
	if s == "" {
		return
	}
	if len(s) > maxLoggedName {
		s = s[:maxLoggedName]
	}
	p.Store(&s)
}

// projectOf is the project m names: its project field, or that of the
// object it carries, as an apply does.
func projectOf(m proto.Message) string {
	if r, ok := m.(interface{ GetProject() string }); ok {
		return r.GetProject()
	}
	return objectMeta(m).GetProject()
}

// nameOf is the name m names: its name field, or that of the object it
// carries.
func nameOf(m proto.Message) string {
	if r, ok := m.(interface{ GetName() string }); ok {
		return r.GetName()
	}
	return objectMeta(m).GetName()
}

// objectMeta is the metadata of the first object m carries, nil for none.
func objectMeta(m proto.Message) *metav1.ObjectMeta {
	var meta *metav1.ObjectMeta
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
			return true
		}
		if o, ok := v.Message().Interface().(interface{ GetMetadata() *metav1.ObjectMeta }); ok {
			meta = o.GetMetadata()
			return false
		}
		return true
	})
	return meta
}

func (a *accessLog) write(ctx context.Context, c *accessCall, procedure string, stream bool, took time.Duration, err error) {
	attrs := []slog.Attr{
		slog.String("procedure", procedure),
		slog.String("code", codeOf(err)),
		slog.Float64("duration_ms", float64(took.Microseconds())/1000),
		slog.Bool("stream", stream),
	}
	p := c.principal.Load()
	if p != nil {
		attrs = append(attrs, slog.String("principal", p.String()), slog.String("principal_type", p.Type.String()))
	}
	tenant := ""
	if t := c.tenant.Load(); t != nil {
		tenant = *t
	}
	if project := c.project.Load(); project != nil {
		attrs = append(attrs, slog.String("project", *project))
		// Only an authenticated caller makes the apiserver look a project up.
		if tenant == "" && p != nil {
			tenant = a.tenantOf(ctx, *project)
		}
	}
	if tenant != "" {
		attrs = append(attrs, slog.String("tenant", tenant))
	}
	if ip := clientIP(ctx); ip != "" {
		attrs = append(attrs, slog.String("client_ip", ip))
	}
	a.s.log.LogAttrs(context.WithoutCancel(ctx), slog.LevelInfo, accessLogMessage, attrs...)
}

// tenantOf is the tenant project is of, empty when there is no such project
// or it cannot be read now.
func (a *accessLog) tenantOf(ctx context.Context, project string) string {
	if t, ok := a.tenants.Load(project); ok {
		return t.(string)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	o, err := a.s.store.Get(ctx, store.Key{Kind: registry.Project.Kind, Name: project})
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.s.log.Warn("reading a project for the access log failed", "project", project, "error", err)
		}
		return ""
	}
	var p tenancyv1alpha1.Project
	if err := proto.Unmarshal(o.Data, &p); err != nil || p.GetSpec().GetTenant() == "" {
		return ""
	}
	a.tenants.Store(project, p.GetSpec().GetTenant())
	return p.GetSpec().GetTenant()
}
