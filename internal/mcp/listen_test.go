package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A client subscribing to tool-list changes must get its subscriptions/listen
// answered at once: held open, it kept a Cloud Run instance billed 24/7.
func TestSubscriptionsListenReturns(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts *mcp.ServerOptions
		held bool
	}{{"sdk default", nil, true}, {"ServerOptions", ServerOptions(), false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, tc.opts)
			mcp.AddTool(s, &mcp.Tool{Name: "noop"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
				return nil, struct{}{}, nil
			})
			srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
				&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
			defer srv.Close()
			body := `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"t","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}},"notifications":{"toolsListChanged":true}}}`
			req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
			req.Header.Set("Mcp-Method", "subscriptions/listen")
			resp, err := (&http.Client{Timeout: time.Second}).Do(req)
			if err == nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			if held := err != nil; held != tc.held {
				t.Fatalf("held = %v (err %v), want %v", held, err, tc.held)
			}
		})
	}
}
