package rpc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/yurifrl/agents/internal/rpc"
)

// fakeWorker is the controllable worker end of a net.Pipe. net.Pipe is
// synchronous, so the worker runs as a background loop: every frame the
// client writes is passed to the handler, and the handler's return value
// (if any) is written back.
type fakeWorker struct {
	conn net.Conn
	t    *testing.T
}

func newPipeRPC(t *testing.T, opts ...rpc.Option) (*rpc.RPC, *fakeWorker) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	worker := &fakeWorker{conn: server, t: t}
	return rpc.New(client, opts...), worker
}

// serve runs the worker loop; handler returns the frames to write back.
func (w *fakeWorker) serve(handler func(frame map[string]any) []string) {
	reader := bufio.NewReader(w.conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}
		for _, out := range handler(frame) {
			if _, err := w.conn.Write([]byte(out)); err != nil {
				return
			}
		}
	}
}

func (w *fakeWorker) write(line string) {
	w.t.Helper()
	if _, err := w.conn.Write([]byte(line + "\n")); err != nil {
		w.t.Errorf("worker write: %v", err)
	}
}

func responseFor(frame map[string]any, extra string) string {
	id, _ := frame["id"].(float64)
	out := fmt.Sprintf(`{"id":%d,"type":"response","success":true`, int(id))
	if extra != "" {
		out += "," + extra
	}
	return out + "}" + "\n"
}

func TestRPCHandshakeAndRequests(t *testing.T) {
	client, worker := newPipeRPC(t)
	defer client.Close()

	events := make(chan rpc.Event, 8)
	client.SetEventHandler(func(e rpc.Event) { events <- e })

	go worker.serve(func(frame map[string]any) []string {
		switch frame["type"] {
		case "get_last_assistant_text":
			return []string{responseFor(frame, `"text":"hi"`)}
		}
		return nil
	})

	// Pre-ready chatter must be ignored; ready completes the handshake.
	worker.write(`{"type":"noise"}`)
	worker.write("not json at all")
	worker.write(`{"type":"ready","protocolVersion":1,"sessionFile":"/s.jsonl"}`)

	ready, err := client.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if ready.ProtocolVersion != 1 || ready.SessionFile != "/s.jsonl" {
		t.Fatalf("ready = %+v", ready)
	}

	resp, err := client.GetLastAssistantText(context.Background())
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.String("text") != "hi" {
		t.Fatalf("resp = %+v", resp)
	}

	// Events dispatch to the handler.
	worker.write(`{"type":"agent_settled"}`)
	select {
	case e := <-events:
		if e["type"] != "agent_settled" {
			t.Fatalf("event = %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered")
	}
}

func TestRPCRequestFailed(t *testing.T) {
	client, worker := newPipeRPC(t)
	defer client.Close()
	go worker.serve(func(frame map[string]any) []string {
		id, _ := frame["id"].(float64)
		return []string{fmt.Sprintf(`{"id":%d,"type":"response","success":false,"error":"nope"}`+"\n", int(id))}
	})
	worker.write(`{"type":"ready"}`)
	if _, err := client.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	_, err := client.GetState(context.Background())
	var rpcErr *rpc.Error
	if !asRPCErr(err, &rpcErr) || rpcErr.Code != rpc.CodeRequestFailed || rpcErr.Message != "nope" {
		t.Fatalf("err = %v (%T)", err, err)
	}
}

func asRPCErr(err error, target **rpc.Error) bool {
	e, ok := err.(*rpc.Error)
	if ok {
		*target = e
	}
	return ok
}

func TestRPCCloseRejectsPending(t *testing.T) {
	client, worker := newPipeRPC(t)
	// Worker reads but never responds.
	go worker.serve(func(map[string]any) []string { return nil })
	worker.write(`{"type":"ready"}`)
	if _, err := client.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := client.GetState(context.Background())
		errc <- err
	}()
	// Let the request land, then close.
	time.Sleep(50 * time.Millisecond)
	client.Close()
	select {
	case err := <-errc:
		var rpcErr *rpc.Error
		if !asRPCErr(err, &rpcErr) || rpcErr.Code != rpc.CodeChannelClosed {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending request not rejected on close")
	}
}

func TestRPCChannelEOFFinishes(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	clientRPC := rpc.New(client)
	server.Write([]byte("{\"type\":\"ready\"}\n"))
	if _, err := clientRPC.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	closed := make(chan struct{})
	clientRPC.OnClose(func() { close(closed) })
	server.Close() // EOF on the client side
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("onClose not fired on EOF")
	}
}

func TestRPCHandshakeTimeout(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	clientRPC := rpc.New(client, rpc.WithHandshakeTimeout(50*time.Millisecond))
	_, err := clientRPC.Start(context.Background())
	var rpcErr *rpc.Error
	if !asRPCErr(err, &rpcErr) || rpcErr.Code != rpc.CodeHandshakeTimeout {
		t.Fatalf("err = %v", err)
	}
}

func TestRPCRequestTimeout(t *testing.T) {
	client, worker := newPipeRPC(t, rpc.WithRequestTimeout(50*time.Millisecond))
	defer client.Close()
	go worker.serve(func(map[string]any) []string { return nil }) // never responds
	worker.write(`{"type":"ready"}`)
	if _, err := client.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	_, err := client.GetState(context.Background())
	var rpcErr *rpc.Error
	if !asRPCErr(err, &rpcErr) || rpcErr.Code != rpc.CodeRequestTimeout {
		t.Fatalf("err = %v", err)
	}
}

func TestRPCStartExactlyOnce(t *testing.T) {
	client, worker := newPipeRPC(t)
	defer client.Close()
	worker.write(`{"type":"ready"}`)
	if _, err := client.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := client.Start(context.Background()); err == nil {
		t.Fatal("second Start must fail")
	}
}
