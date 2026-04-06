package proxy

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"go.uber.org/zap"
)

// errorPageTmpl is a minimal Cloudflare-style HTML error page embedded in
// the binary.  No external assets are required.
var errorPageTmpl = template.Must(template.New("error").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.Code}} {{.Title}}</title>
  <style>
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: #f4f4f5; color: #18181b; display: flex; align-items: center;
      justify-content: center; min-height: 100vh; margin: 0;
    }
    .card {
      background: #fff; border-radius: 12px; box-shadow: 0 2px 8px rgba(0,0,0,.08);
      padding: 48px 40px; text-align: center; max-width: 480px; width: 100%;
    }
    .code { font-size: 72px; font-weight: 700; color: #dc2626; margin: 0; }
    .title { font-size: 20px; margin: 8px 0 16px; }
    .detail { color: #71717a; font-size: 14px; margin: 0 0 24px; }
    .footer { color: #a1a1aa; font-size: 12px; }
  </style>
</head>
<body>
  <div class="card">
    <p class="code">{{.Code}}</p>
    <p class="title">{{.Title}}</p>
    <p class="detail">{{.Detail}}</p>
    <p class="footer">Caravanserai Proxy</p>
  </div>
</body>
</html>`))

// Server is an HTTP reverse proxy that routes requests to containers based
// on the Host header using routes maintained in a RouteTable.
type Server struct {
	httpServer *http.Server
	routes     *RouteTable
	logger     *zap.Logger
}

// NewServer creates a proxy Server listening on listenAddr (e.g. ":8081").
func NewServer(logger *zap.Logger, listenAddr string, routes *RouteTable) *Server {
	s := &Server{
		routes: routes,
		logger: logger,
	}

	s.httpServer = &http.Server{
		Addr:              listenAddr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	return s
}

// ListenAndServe starts the proxy server.  It blocks until the server is
// shut down or an error occurs.
func (s *Server) ListenAndServe() error {
	s.logger.Info("Proxy server listening", zap.String("addr", s.httpServer.Addr))
	err := s.httpServer.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown gracefully shuts down the proxy server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// handler returns the http.Handler for the proxy server.
func (s *Server) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host

		backendURL, found := s.routes.Lookup(host)
		if !found {
			s.writeErrorPage(w, 502, "Bad Gateway",
				fmt.Sprintf("Host %q has no backend available.", host))
			return
		}

		target, err := url.Parse(backendURL)
		if err != nil {
			s.logger.Error("Failed to parse backend URL",
				zap.String("host", host),
				zap.String("backend", backendURL),
				zap.Error(err),
			)
			s.writeErrorPage(w, 502, "Bad Gateway", "Invalid backend configuration.")
			return
		}

		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = pr.In.Host

				// Set standard proxy headers.
				pr.SetXForwarded()
			},
			ErrorHandler: func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
				s.logger.Warn("Proxy error",
					zap.String("host", req.Host),
					zap.String("backend", backendURL),
					zap.Error(proxyErr),
				)
				s.writeErrorPage(rw, 502, "Bad Gateway",
					fmt.Sprintf("Backend %q is unreachable.", host))
			},
		}

		proxy.ServeHTTP(w, r)
	})
}

// writeErrorPage renders the Cloudflare-style HTML error page.
func (s *Server) writeErrorPage(w http.ResponseWriter, code int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)

	data := struct {
		Code   int
		Title  string
		Detail string
	}{
		Code:   code,
		Title:  title,
		Detail: detail,
	}

	if err := errorPageTmpl.Execute(w, data); err != nil {
		s.logger.Error("Failed to render error page", zap.Error(err))
	}
}
