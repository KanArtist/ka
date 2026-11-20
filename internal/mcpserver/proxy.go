package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Proxy bridges a stdio MCP client to a daemon's Streamable HTTP endpoint
// (PRD §8.3), so `ka mcp` shares the daemon's index, presence and events.
// Messages pass through unchanged: the client's initialize reaches the
// daemon, which derives the agent's actor from clientInfo exactly as the
// in-process server does.
type Proxy struct {
	Endpoint string      // http://127.0.0.1:7711/mcp
	Token    string      // the daemon's local token
	Header   http.Header // extra headers (X-Ka-Actor, X-Ka-Session)
	Client   *http.Client

	mu       sync.Mutex
	session  string
	version  string
	init     []byte // the client's initialize, to start a new session with
	renewMu  sync.Mutex
	outMu    sync.Mutex
	inflight sync.WaitGroup
}

// closeTimeout bounds ending the HTTP session when the client goes away.
const closeTimeout = 5 * time.Second

// Run copies messages until in reaches EOF or ctx is done, then ends the
// HTTP session.
func (p *Proxy) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	if p.Client == nil {
		p.Client = &http.Client{}
	}
	rd := bufio.NewReaderSize(in, 64*1024)
	readErr := make(chan error, 1)
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		for {
			line, err := rd.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					readErr <- err
				}
				return
			}
		}
	}()
	first := true
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case line, ok := <-lines:
			if !ok {
				break loop
			}
			msg := bytes.TrimSpace(line)
			if first {
				// initialize must finish (and give us the session ID) before
				// anything else is sent.
				first = false
				p.forward(ctx, msg, out)
				continue
			}
			p.inflight.Add(1)
			go func() {
				defer p.inflight.Done()
				p.forward(ctx, msg, out)
			}()
		}
	}
	p.inflight.Wait()
	p.close()
	select {
	case err := <-readErr:
		return err
	default: // EOF, or ctx ended with the reader still blocked
		return nil
	}
}

type rpcHead struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result *struct {
		ProtocolVersion string `json:"protocolVersion"`
	} `json:"result,omitempty"`
}

// headers sets the request's headers and returns the session it is for.
func (p *Proxy) headers(req *http.Request) string {
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, vs := range p.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	p.mu.Lock()
	if p.session != "" {
		req.Header.Set("Mcp-Session-Id", p.session)
	}
	if p.version != "" {
		req.Header.Set("Mcp-Protocol-Version", p.version)
	}
	sid := p.session
	p.mu.Unlock()
	return sid
}

// post sends one message, and records the session the daemon assigns.
func (p *Proxy) post(ctx context.Context, msg []byte) (*http.Response, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint, bytes.NewReader(msg))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	sid := p.headers(req)
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, "", err
	}
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		p.mu.Lock()
		if p.session == "" {
			p.session = s
		}
		p.mu.Unlock()
	}
	return resp, sid, nil
}

// renew starts a new session after the daemon ended the stale one (it
// expires idle sessions), by replaying the client's initialize. Requests
// that hit the same expiry meanwhile renew it once and send again.
func (p *Proxy) renew(ctx context.Context, stale string) error {
	p.renewMu.Lock()
	defer p.renewMu.Unlock()
	p.mu.Lock()
	cur, init := p.session, p.init
	p.mu.Unlock()
	switch {
	case cur != stale:
		return nil // already renewed
	case init == nil:
		return errors.New("the daemon ended the session")
	}
	sid := ""
	for _, msg := range [][]byte{init, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint, bytes.NewReader(msg))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		p.headers(req)
		if sid == "" {
			req.Header.Del("Mcp-Session-Id")
			req.Header.Del("Mcp-Protocol-Version")
		} else {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := p.Client.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("starting a new session: HTTP %d", resp.StatusCode)
		}
		if sid == "" {
			if sid = resp.Header.Get("Mcp-Session-Id"); sid == "" {
				return errors.New("starting a new session: no session ID")
			}
		}
	}
	p.mu.Lock()
	p.session = sid
	p.mu.Unlock()
	return nil
}

// sameID reports whether two JSON-RPC IDs are equal (1 and 1.0 are).
func sameID(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// forward POSTs one message and writes every message of the response.
func (p *Proxy) forward(ctx context.Context, msg []byte, out io.Writer) {
	var head rpcHead
	json.Unmarshal(msg, &head)
	isRequest := head.Method != "" && len(head.ID) > 0 && string(head.ID) != "null"

	if head.Method == "initialize" {
		p.mu.Lock()
		p.init = bytes.Clone(msg)
		p.mu.Unlock()
	}
	resp, sid, err := p.post(ctx, msg)
	if err == nil && resp.StatusCode == http.StatusNotFound && sid != "" && head.Method != "initialize" {
		// The daemon expired the session: start another and send again.
		resp.Body.Close()
		if err = p.renew(ctx, sid); err == nil {
			resp, _, err = p.post(ctx, msg)
		}
	}
	if err != nil {
		p.fail(out, head, isRequest, err)
		return
	}
	defer resp.Body.Close()
	// answered notes a response to this request; if the stream ends without
	// one, the client gets an error instead of waiting forever.
	answered := !isRequest
	emit := func(m []byte) {
		if id, isResponse := p.emit(out, m); isResponse && sameID(id, head.ID) {
			answered = true
		}
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return // a notification or response was accepted
	case resp.StatusCode >= 300:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		p.fail(out, head, isRequest, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
		return
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 64<<20)
		var data bytes.Buffer
		for sc.Scan() {
			line := sc.Bytes()
			switch {
			case len(line) == 0:
				if data.Len() > 0 {
					emit(data.Bytes())
					data.Reset()
				}
			case bytes.HasPrefix(line, []byte("data:")):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
			}
		}
		if data.Len() > 0 {
			emit(data.Bytes())
		}
		if !answered {
			err := sc.Err()
			if err == nil {
				err = errors.New("the response stream ended without an answer")
			}
			p.fail(out, head, isRequest, err)
		}
		return
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		p.fail(out, head, isRequest, err)
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		emit(body)
	}
	if !answered {
		p.fail(out, head, isRequest, errors.New("the response carried no answer"))
	}
}

// emit writes one message to the client, noting the negotiated protocol
// version from the initialize result. It returns the message's ID and
// whether it is a response (not a request from the server).
func (p *Proxy) emit(out io.Writer, msg []byte) (json.RawMessage, bool) {
	var head rpcHead
	ok := json.Unmarshal(msg, &head) == nil
	if ok && head.Result != nil && head.Result.ProtocolVersion != "" {
		p.mu.Lock()
		if p.version == "" {
			p.version = head.Result.ProtocolVersion
		}
		p.mu.Unlock()
	}
	var buf bytes.Buffer
	if json.Compact(&buf, msg) != nil {
		return nil, false
	}
	buf.WriteByte('\n')
	p.outMu.Lock()
	out.Write(buf.Bytes())
	p.outMu.Unlock()
	return head.ID, ok && head.Method == ""
}

// fail answers a request whose forwarding failed, so the client is not left
// waiting.
func (p *Proxy) fail(out io.Writer, head rpcHead, isRequest bool, err error) {
	if !isRequest {
		return
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": head.ID, "error": map[string]any{
		"code": -32603, "message": "ka daemon unreachable (" + err.Error() + "); restart the MCP server, or run `ka mcp --no-daemon`",
	}}
	b, _ := json.Marshal(resp)
	p.emit(out, b)
}

// close ends the HTTP session.
func (p *Proxy) close() {
	p.mu.Lock()
	sid := p.session
	p.mu.Unlock()
	if sid == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, p.Endpoint, nil)
	if err != nil {
		return
	}
	p.headers(req)
	if resp, err := p.Client.Do(req); err == nil {
		resp.Body.Close()
	}
}
