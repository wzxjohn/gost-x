package v5

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/go-gost/core/handler"
	"github.com/go-gost/core/limiter/traffic"
	md "github.com/go-gost/core/metadata"
	"github.com/go-gost/core/recorder"
	"github.com/go-gost/gosocks5"
	ctxvalue "github.com/go-gost/x/ctx"
	limiter_util "github.com/go-gost/x/internal/util/limiter"
	"github.com/go-gost/x/internal/util/socks"
	stats_util "github.com/go-gost/x/internal/util/stats"
	rate_limiter "github.com/go-gost/x/limiter/rate"
	xrecorder "github.com/go-gost/x/recorder"
	"github.com/go-gost/x/registry"
)

var (
	ErrUnknownCmd = errors.New("socks5: unknown command")
)

func init() {
	registry.HandlerRegistry().Register("socks5", NewHandler)
	registry.HandlerRegistry().Register("socks", NewHandler)
}

type socks5Handler struct {
	selector gosocks5.Selector
	md       metadata
	options  handler.Options
	stats    *stats_util.HandlerStats
	limiter  traffic.TrafficLimiter
	cancel   context.CancelFunc
	recorder recorder.RecorderObject
}

func NewHandler(opts ...handler.Option) handler.Handler {
	options := handler.Options{}
	for _, opt := range opts {
		opt(&options)
	}

	return &socks5Handler{
		options: options,
	}
}

func (h *socks5Handler) Init(md md.Metadata) (err error) {
	if err = h.parseMetadata(md); err != nil {
		return
	}

	h.selector = &serverSelector{
		Authenticator: h.options.Auther,
		TLSConfig:     h.options.TLSConfig,
		logger:        h.options.Logger,
		noTLS:         h.md.noTLS,
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel

	if h.options.Observer != nil {
		h.stats = stats_util.NewHandlerStats(h.options.Service)
		go h.observeStats(ctx)
	}

	if limiter := h.options.Limiter; limiter != nil {
		h.limiter = limiter_util.NewCachedTrafficLimiter(limiter, 30*time.Second, 60*time.Second)
	}

	for _, ro := range h.options.Recorders {
		if ro.Record == xrecorder.RecorderServiceHandler {
			h.recorder = ro
			break
		}
	}

	return
}

func (h *socks5Handler) Handle(ctx context.Context, conn net.Conn, opts ...handler.HandleOption) (err error) {
	defer conn.Close()

	start := time.Now()

	ro := &xrecorder.HandlerRecorderObject{
		Service:    h.options.Service,
		Network:    "tcp",
		RemoteAddr: conn.RemoteAddr().String(),
		LocalAddr:  conn.LocalAddr().String(),
		Time:       start,
		SID:        string(ctxvalue.SidFromContext(ctx)),
	}

	log := h.options.Logger.WithFields(map[string]any{
		"remote": conn.RemoteAddr().String(),
		"local":  conn.LocalAddr().String(),
		"sid":    ctxvalue.SidFromContext(ctx),
	})

	log.Infof("%s <> %s", conn.RemoteAddr(), conn.LocalAddr())
	defer func() {
		if err != nil {
			ro.Err = err.Error()
		}
		ro.Duration = time.Since(start)
		ro.Record(ctx, h.recorder.Recorder)

		log.WithFields(map[string]any{
			"duration": time.Since(start),
		}).Infof("%s >< %s", conn.RemoteAddr(), conn.LocalAddr())
	}()

	if !h.checkRateLimit(conn.RemoteAddr()) {
		return rate_limiter.ErrRateLimit
	}

	if h.md.readTimeout > 0 {
		conn.SetReadDeadline(time.Now().Add(h.md.readTimeout))
	}

	sc := gosocks5.ServerConn(conn, h.selector)
	req, err := gosocks5.ReadRequest(sc)
	if err != nil {
		log.Error(err)
		return err
	}
	log.Trace(req)

	if clientID := sc.ID(); clientID != "" {
		ctx = ctxvalue.ContextWithClientID(ctx, ctxvalue.ClientID(clientID))
		log = log.WithFields(map[string]any{"user": clientID})
		ro.Client = clientID
	}

	conn = sc
	conn.SetReadDeadline(time.Time{})

	address := req.Addr.String()
	ro.Host = address

	switch req.Cmd {
	case gosocks5.CmdConnect:
		return h.handleConnect(ctx, conn, "tcp", address, log)
	case gosocks5.CmdBind:
		return h.handleBind(ctx, conn, "tcp", address, log)
	case socks.CmdMuxBind:
		return h.handleMuxBind(ctx, conn, "tcp", address, log)
	case gosocks5.CmdUdp:
		ro.Network = "udp"
		return h.handleUDP(ctx, conn, log)
	case socks.CmdUDPTun:
		ro.Network = "udp"
		return h.handleUDPTun(ctx, conn, "udp", address, log)
	default:
		err = ErrUnknownCmd
		log.Error(err)
		resp := gosocks5.NewReply(gosocks5.CmdUnsupported, nil)
		log.Trace(resp)
		resp.Write(conn)
		return err
	}
}

func (h *socks5Handler) Close() error {
	if h.cancel != nil {
		h.cancel()
	}
	return nil
}

func (h *socks5Handler) checkRateLimit(addr net.Addr) bool {
	if h.options.RateLimiter == nil {
		return true
	}
	host, _, _ := net.SplitHostPort(addr.String())
	if limiter := h.options.RateLimiter.Limiter(host); limiter != nil {
		return limiter.Allow(1)
	}

	return true
}

func (h *socks5Handler) observeStats(ctx context.Context) {
	if h.options.Observer == nil {
		return
	}

	d := h.md.observePeriod
	if d < time.Millisecond {
		d = 5 * time.Second
	}
	ticker := time.NewTicker(d)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.options.Observer.Observe(ctx, h.stats.Events())
		case <-ctx.Done():
			return
		}
	}
}
