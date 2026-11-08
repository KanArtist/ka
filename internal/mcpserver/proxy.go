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
	"strings"
	"sync"
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
	outMu    sync.Mutex
	inflight sync.WaitGroup
}

// Run copies messages until in reaches EOF or ctx is done, then ends the
// HTTP session.
func (p *Proxy) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	if p.Client == nil {
		p.Client = &http.Client{}
	}
	rd := bufio.NewReaderSize(in, 64*1024)
	var readErr error
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
					readErr = err
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
	return readErr
}

type rpcHead struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result *struct {
		ProtocolVersion string `json:"protocolVersion"`
	} `json:"result,omitempty"`
}

func (p *Proxy) headers(req *http.Request) {
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
	p.mu.Unlock()
}

// forward POSTs one message and writes every message of the response.
func (p *Proxy) forward(ctx context.Context, msg []byte, out io.Writer) {
	var head rpcHead
	json.Unmarshal(msg, &head)
	isRequest := head.Method != "" && len(head.ID) > 0 && string(head.ID) != "null"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint, bytes.NewReader(msg))
	if err != nil {
		p.fail(out, head, isRequest, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	p.headers(req)
	resp, err := p.Client.Do(req)
	if err != nil {
		p.fail(out, head, isRequest, err)
		return
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		p.mu.Lock()
		if p.session == "" {
			p.session = sid
		}
		p.mu.Unlock()
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
					p.emit(out, data.Bytes())
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
			p.emit(out, data.Bytes())
		}
		return
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		p.fail(out, head, isRequest, err)
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		p.emit(out, body)
	}
}

// emit writes one message to the client, noting the negotiated protocol
// version from the initialize result.
func (p *Proxy) emit(out io.Writer, msg []byte) {
	var head rpcHead
	if json.Unmarshal(msg, &head) == nil && head.Result != nil && head.Result.ProtocolVersion != "" {
		p.mu.Lock()
		if p.version == "" {
			p.version = head.Result.ProtocolVersion
		}
		p.mu.Unlock()
	}
	var buf bytes.Buffer
	if json.Compact(&buf, msg) != nil {
		return
	}
	buf.WriteByte('\n')
	p.outMu.Lock()
	out.Write(buf.Bytes())
	p.outMu.Unlock()
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
	req, err := http.NewRequest(http.MethodDelete, p.Endpoint, nil)
	if err != nil {
		return
	}
	p.headers(req)
	if resp, err := p.Client.Do(req); err == nil {
		resp.Body.Close()
	}
}
