package bleephub

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
)

// durabilityBarrierMiddleware makes group commit safe to expose: it withholds a
// mutating request's response until the writes that request produced are durable.
//
// With group commit, a store write returns as soon as its ops are enqueued — the
// fsync happens later, off the caller's Store.Mu, batched with other writers. In
// memory the mutation is already visible, but it is not yet on disk. This
// middleware buffers the response, and after the handler returns waits for the
// group committer to fsync past the point this request reached before flushing.
// So an acknowledged (2xx) write is exactly as durable as it was synchronously; a
// crash can only drop writes whose response never reached a client. A commit
// failure turns into a 500 and a reload from the durable state.
//
// It is a no-op unless group commit is active, and only engages for mutating,
// non-byte-transfer requests — GETs enqueue nothing, and streaming/byte-transfer
// routes must not be buffered.
func (s *Server) durabilityBarrierMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := s.store.Persist
		if !p.GroupCommitActive() || !isMutatingMethod(r.Method) || isByteTransferRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		seqBefore := p.EnqueuedSeq()
		bw := &durabilityWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(bw, r)
		if bw.streamed {
			// The handler streamed (hijack or explicit Flush); durability for
			// those paths is not gated here.
			return
		}
		seqAfter := p.EnqueuedSeq()
		if seqAfter != seqBefore {
			if err := p.WaitDurable(r.Context(), seqAfter); err != nil {
				if r.Context().Err() == nil {
					// The write could not be made durable: drop the in-memory
					// state that raced ahead of disk and fail the request.
					_ = s.store.ReloadFromPersistence()
					writeGHError(w, http.StatusInternalServerError, "The server could not persist this change")
				}
				return
			}
		}
		bw.flush()
	})
}

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// durabilityWriter buffers status and body so the barrier can hold a response
// until it is durable. Header() passes through to the real writer (net/http does
// not send headers until WriteHeader/Write), so buffered headers are emitted at
// flush. A Flush or Hijack means the handler wants the bytes now: the writer
// switches to pass-through and the barrier stops gating it.
type durabilityWriter struct {
	http.ResponseWriter
	status   int
	body     []byte
	wrote    bool
	streamed bool
}

func (w *durabilityWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	if w.streamed {
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *durabilityWriter) Write(b []byte) (int, error) {
	if w.streamed {
		return w.ResponseWriter.Write(b)
	}
	w.wrote = true
	w.body = append(w.body, b...)
	return len(b), nil
}

// flush emits the buffered response. A body-bearing response gets an explicit
// Content-Length (the length net/http would have computed for a single buffered
// write) so the wire framing matches the synchronous path.
func (w *durabilityWriter) flush() {
	if w.streamed {
		return
	}
	if len(w.body) > 0 && w.status != http.StatusNoContent && w.status != http.StatusNotModified &&
		w.Header().Get("Content-Length") == "" && w.Header().Get("Transfer-Encoding") == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(w.body)))
	}
	w.ResponseWriter.WriteHeader(w.status)
	if len(w.body) > 0 {
		_, _ = w.ResponseWriter.Write(w.body)
	}
}

// Flush switches to streaming: emit whatever is buffered and forward future
// writes directly. Buffered responses that never Flush are gated by the barrier.
func (w *durabilityWriter) Flush() {
	if !w.streamed {
		w.streamed = true
		w.ResponseWriter.WriteHeader(w.status)
		if len(w.body) > 0 {
			_, _ = w.ResponseWriter.Write(w.body)
			w.body = nil
		}
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *durabilityWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
	}
	w.streamed = true
	return h.Hijack()
}
