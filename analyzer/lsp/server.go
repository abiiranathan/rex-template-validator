package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Handler is the function signature for a JSON-RPC method handler.
// A nil *ResponseError means success.
type Handler func(params json.RawMessage) (any, *ResponseError)

// Server is a JSON-RPC 2.0 server that reads from stdin and writes to stdout,
// implementing the LSP wire protocol (Content-Length framing).
//
// All rex/* methods are handled here. Standard LSP lifecycle methods
// (initialize, shutdown, exit) are also handled minimally so that any
// LSP-aware editor or test harness can communicate with the daemon.
type Server struct {
	reader  *bufio.Reader
	writer  io.Writer
	writeMu sync.Mutex

	handlers map[string]Handler
	cache    *analysisCache

	// shutdown is set to 1 by the "shutdown" request; "exit" then terminates.
	shutdown atomic.Int32
}

// NewServer creates a fully initialised Server ready to call Serve().
func NewServer() *Server {
	s := &Server{
		reader:   bufio.NewReaderSize(os.Stdin, 1<<20), // 1 MiB read buffer
		writer:   os.Stdout,
		handlers: make(map[string]Handler),
		cache:    newAnalysisCache(),
	}
	s.registerHandlers()
	return s
}

// Serve enters the main read / dispatch / write loop.
// It returns only when stdin is closed or an unrecoverable read error occurs.
func (s *Server) Serve() {
	for {
		raw, err := s.readFrame()
		if err != nil {
			if err == io.EOF {
				return
			}
			log.Printf("lsp: read error: %v", err)
			return
		}

		// Dispatch every message in its own goroutine so slow analyses do not
		// head-of-line block cheaper requests (e.g. getTemplateContext while
		// a full validate is in flight for a large project).
		go s.dispatch(raw)
	}
}

// ─── Wire-format read ─────────────────────────────────────────────────────────

// readFrame reads one LSP message from stdin.
// LSP framing: one or more "Header: value\r\n" lines, a blank "\r\n" line,
// then exactly Content-Length bytes of JSON body.
func (s *Server) readFrame() (json.RawMessage, error) {
	contentLength := 0

	// Read headers until blank line.
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if after, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			val := after
			contentLength, err = strconv.Atoi(strings.TrimSpace(val))
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length: %w", err)
			}
		}
		// Content-Type and other headers are intentionally ignored.
	}

	if contentLength <= 0 {
		return nil, fmt.Errorf("missing or zero Content-Length")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(s.reader, body); err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}

	return json.RawMessage(body), nil
}

// ─── Dispatch ────────────────────────────────────────────────────────────────

// dispatch parses a raw JSON-RPC message and routes it to the right handler.
func (s *Server) dispatch(raw json.RawMessage) {
	// Minimal parse: we only need id, method, and params up-front.
	var base struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}

	if err := json.Unmarshal(raw, &base); err != nil {
		s.sendError(nil, ErrParse, "parse error: "+err.Error(), nil)
		return
	}

	// A message is a notification when it has no id field (or id == null).
	isNotification := len(base.ID) == 0 || string(base.ID) == "null"

	// ── Built-in lifecycle methods ────────────────────────────────────────

	switch base.Method {
	case "shutdown":
		s.shutdown.Store(1)
		if !isNotification {
			s.sendResult(base.ID, nil)
		}
		return

	case "exit":
		if s.shutdown.Load() == 1 {
			os.Exit(0)
		}
		os.Exit(1)

	case "initialized", "$/cancelRequest":
		// No-op notifications required by the LSP spec.
		return
	}

	// ── Registered handlers ───────────────────────────────────────────────

	handler, ok := s.handlers[base.Method]
	if !ok {
		if !isNotification {
			s.sendError(base.ID, ErrMethodNotFound,
				fmt.Sprintf("method not found: %s", base.Method), nil)
		}
		return
	}

	result, rpcErr := handler(base.Params)

	// Notifications do not receive a response.
	if isNotification {
		return
	}

	if rpcErr != nil {
		s.sendError(base.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
		return
	}

	s.sendResult(base.ID, result)
}

// ─── Response writers ────────────────────────────────────────────────────────

func (s *Server) sendResult(id json.RawMessage, result any) {
	s.writeMessage(ResponseMessage{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Result:  result,
	})
}

func (s *Server) sendError(id json.RawMessage, code int, message string, data any) {
	s.writeMessage(ResponseMessage{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error: &ResponseError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	})
}

// writeMessage serialises msg as JSON and writes it with LSP framing.
// The mutex ensures that concurrent goroutines never interleave their output.
func (s *Server) writeMessage(msg any) {
	body, err := json.Marshal(msg)
	if err != nil {
		log.Printf("lsp: marshal error: %v", err)
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// LSP wire format: headers terminated by \r\n\r\n, then raw JSON body.
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := io.WriteString(s.writer, header); err != nil {
		log.Printf("lsp: write header error: %v", err)
		return
	}
	if _, err := s.writer.Write(body); err != nil {
		log.Printf("lsp: write body error: %v", err)
	}
}
