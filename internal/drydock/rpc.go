package drydock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// sinkFunc adapts a function to the Sink interface.
type sinkFunc func(Event) error

func (f sinkFunc) Emit(e Event) error { return f(e) }

// --- portal server --------------------------------------------------------

// Server serves a Driver over HTTP as NDJSON streams, JSON unary results, and
// PNG screenshots.
type Server struct {
	Driver       Driver
	Token        string
	PathMap      [][2]string
	AllowNixArgs bool
}

// Handler returns the portal's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name":    "drydock-portal",
			"version": Version,
			"methods": []string{"up", "apply", "reset", "down", "status", "url", "screenshot", "ssh", "logs", "list", "doctor"},
		})
	})

	mux.HandleFunc("/rpc/up", s.stream(s.Driver.Up))
	mux.HandleFunc("/rpc/apply", s.stream(s.Driver.Apply))
	mux.HandleFunc("/rpc/reset", s.stream(s.Driver.Reset))
	mux.HandleFunc("/rpc/down", s.stream(s.Driver.Down))
	mux.HandleFunc("/rpc/logs", s.stream(s.Driver.Logs))
	mux.HandleFunc("/rpc/doctor", s.stream(func(ctx context.Context, req Request, sink Sink) error {
		return s.Driver.Doctor(ctx, sink)
	}))
	mux.HandleFunc("/rpc/ssh", func(w http.ResponseWriter, r *http.Request) {
		req, err := s.decodeRequest(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if len(req.Args) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "interactive ssh is only available from a local drydock; pass -- CMD",
			})
			return
		}
		s.streamRequest(w, r, req, s.Driver.SSH)
	})

	mux.HandleFunc("/rpc/status", s.unary(func(ctx context.Context, req Request) (any, error) {
		return s.Driver.Status(ctx, req)
	}))
	mux.HandleFunc("/rpc/url", s.unary(func(ctx context.Context, req Request) (any, error) {
		url, err := s.Driver.URL(ctx, req)
		if err != nil {
			return nil, err
		}
		return map[string]string{"url": url}, nil
	}))
	mux.HandleFunc("/rpc/list", s.unary(func(ctx context.Context, req Request) (any, error) {
		return s.Driver.List(ctx)
	}))
	mux.HandleFunc("/rpc/screenshot", func(w http.ResponseWriter, r *http.Request) {
		req, err := s.decodeRequest(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		data, err := s.Driver.Screenshot(r.Context(), req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})

	return s.auth(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" && r.Header.Get("Authorization") != "Bearer "+s.Token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) stream(fn func(context.Context, Request, Sink) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := s.decodeRequest(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.streamRequest(w, r, req, fn)
	}
}

// streamRequest serves an already-decoded request as an NDJSON event stream.
func (s *Server) streamRequest(w http.ResponseWriter, r *http.Request, req Request, fn func(context.Context, Request, Sink) error) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	sink := sinkFunc(func(e Event) error {
		if err := encoder.Encode(e); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err := fn(r.Context(), req, sink); err != nil {
		_ = encoder.Encode(Event{Type: "error", Data: err.Error()})
	} else {
		_ = encoder.Encode(Event{Type: "done"})
	}
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) unary(fn func(context.Context, Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := s.decodeRequest(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := fn(r.Context(), req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func decodeRequest(r *http.Request) (Request, error) {
	var req Request
	if r.Body == nil {
		return req, nil
	}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil && !errors.Is(err, io.EOF) {
		return req, err
	}
	return req, nil
}

// prepare rewrites guest-visible paths. Defaults live on the Engine so that
// saved per-VM state wins over them.
func (s *Server) prepare(req *Request) {
	req.Flake = applyPathMap(req.Flake, s.PathMap)
	for i := range req.Overrides {
		req.Overrides[i].Ref = applyPathMap(req.Overrides[i].Ref, s.PathMap)
	}
}

func (s *Server) decodeRequest(r *http.Request) (Request, error) {
	req, err := decodeRequest(r)
	if err != nil {
		return req, err
	}
	s.prepare(&req)
	if !s.AllowNixArgs && len(req.NixArgs) > 0 {
		return req, fmt.Errorf("this portal does not allow --nix-arg")
	}
	return req, nil
}

func applyPathMap(ref string, mapping [][2]string) string {
	prefix, value := "", ref
	if strings.HasPrefix(ref, "path:") {
		prefix, value = "path:", ref[len("path:"):]
	}
	for _, pair := range mapping {
		from, to := pair[0], pair[1]
		if value == from {
			return prefix + to
		}
		if strings.HasPrefix(value, from+"/") {
			return prefix + to + value[len(from):]
		}
	}
	return ref
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

// --- remote client --------------------------------------------------------

// Remote is a Driver backed by a drydock portal.
type Remote struct {
	BaseURL string
	Token   string
}

// NewRemote builds a Remote from a portal URL and optional token.
func NewRemote(baseURL, token string) *Remote {
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	return &Remote{BaseURL: strings.TrimRight(baseURL, "/"), Token: token}
}

func (c *Remote) request(ctx context.Context, method string, req Request) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/rpc/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		message := strings.TrimSpace(string(data))
		var envelope struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &envelope) == nil && envelope.Error != "" {
			message = envelope.Error
		}
		return nil, fmt.Errorf("%s: %s", resp.Status, message)
	}
	return resp, nil
}

func (c *Remote) stream(ctx context.Context, method string, req Request, sink Sink) error {
	resp, err := c.request(ctx, method, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)
	for {
		var event Event
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch event.Type {
		case "error":
			return errors.New(event.Data)
		case "done":
			return nil
		default:
			if sink != nil {
				if err := sink.Emit(event); err != nil {
					return err
				}
			}
		}
	}
}

func (c *Remote) unary(ctx context.Context, method string, req Request, out any) error {
	resp, err := c.request(ctx, method, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Remote) Up(ctx context.Context, req Request, sink Sink) error {
	return c.stream(ctx, "up", req, sink)
}
func (c *Remote) Apply(ctx context.Context, req Request, sink Sink) error {
	return c.stream(ctx, "apply", req, sink)
}
func (c *Remote) Reset(ctx context.Context, req Request, sink Sink) error {
	return c.stream(ctx, "reset", req, sink)
}
func (c *Remote) Down(ctx context.Context, req Request, sink Sink) error {
	return c.stream(ctx, "down", req, sink)
}
func (c *Remote) SSH(ctx context.Context, req Request, sink Sink) error {
	return c.stream(ctx, "ssh", req, sink)
}
func (c *Remote) Logs(ctx context.Context, req Request, sink Sink) error {
	return c.stream(ctx, "logs", req, sink)
}
func (c *Remote) Doctor(ctx context.Context, sink Sink) error {
	return c.stream(ctx, "doctor", Request{}, sink)
}
func (c *Remote) Status(ctx context.Context, req Request) (Status, error) {
	var status Status
	err := c.unary(ctx, "status", req, &status)
	return status, err
}
func (c *Remote) URL(ctx context.Context, req Request) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	err := c.unary(ctx, "url", req, &out)
	return out.URL, err
}
func (c *Remote) List(ctx context.Context) ([]VMInfo, error) {
	var out []VMInfo
	err := c.unary(ctx, "list", Request{}, &out)
	return out, err
}
func (c *Remote) Screenshot(ctx context.Context, req Request) ([]byte, error) {
	resp, err := c.request(ctx, "screenshot", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
