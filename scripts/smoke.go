//go:build ignore

// Smoke script: build, start the server, drive the Cut A surface over the wire,
// print every response, then stop the server. Run with:
//
//	go run scripts\smoke.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"
)

func main() {
	exe := "bin\\hindsight-go.exe"
	addr := "127.0.0.1:8893"
	base := "http://" + addr

	build := exec.Command("go", "build", "-o", exe, "./cmd/hindsight-go")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Println("build failed:", string(out))
		os.Exit(1)
	}
	fmt.Println("build OK")

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println("port busy:", err)
		os.Exit(1)
	}
	_ = ln.Close()

	cmd := exec.Command(exe, "-addr", addr)
	if err := cmd.Start(); err != nil {
		fmt.Println("start:", err)
		os.Exit(1)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	time.Sleep(700 * time.Millisecond)

	steps := []struct {
		note   string
		method string
		path   string
		body   any
	}{
		{"health", "GET", "/health", nil},
		{"version", "GET", "/version", nil},
		{"PUT bank", "PUT", "/v1/default/banks/demo", map[string]any{"reflect_mission": "demo"}},
		{"retain", "POST", "/v1/default/banks/demo/memories", map[string]any{
			"items": []any{
				map[string]any{"content": "The launch went well and users loved the demo."},
				map[string]any{"content": "Follow up with the design team about onboarding.", "tags": []string{"todo"}},
			},
		}},
		{"recall", "POST", "/v1/default/banks/demo/memories/recall", map[string]any{"query": "how did the launch go?"}},
		{"tags", "GET", "/v1/default/banks/demo/tags", nil},
		{"unimplemented (501)", "GET", "/v1/default/banks/demo/mental-models", nil},
	}

	for _, s := range steps {
		var rdr io.Reader
		if s.body != nil {
			raw, _ := json.Marshal(s.body)
			rdr = bytes.NewReader(raw)
		}
		req, err := http.NewRequest(s.method, base+s.path, rdr)
		if err != nil {
			fmt.Println("request:", err)
			os.Exit(1)
		}
		if s.body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println(s.note, "ERR:", err)
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		fmt.Printf("%-22s %d  %s\n", s.note, resp.StatusCode, string(raw))
	}
}
