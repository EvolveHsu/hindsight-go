//go:build ignore

// Real-LLM end-to-end: loads .env.local, starts the server binary, drives the
// full loop over the wire (bank -> retain -> consolidate -> reflect ->
// mental-model), prints every response. Run: go run scripts\e2e_real.go
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

func loadDotEnv(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, "="); i > 0 {
			out[line[:i]] = line[i+1:]
		}
	}
	return out
}

func main() {
	env := loadDotEnv(".env.local")
	if env["HINDSIGHT_LITE_LLM_BASE_URL"] == "" {
		fmt.Println(".env.local missing HINDSIGHT_LITE_LLM_BASE_URL")
		os.Exit(1)
	}
	fmt.Println("LLM:", env["HINDSIGHT_LITE_LLM_BASE_URL"], "model:", env["HINDSIGHT_LITE_LLM_MODEL"])

	exe := "bin\\hindsight-go.exe"
	addr := "127.0.0.1:8895"
	base := "http://" + addr

	build := exec.Command("go", "build", "-o", exe, "./cmd/hindsight-go")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Println("build:", string(out))
		os.Exit(1)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println("port busy:", err)
		os.Exit(1)
	}
	ln.Close()

	cmd := exec.Command(exe, "-addr", addr)
	cmd.Env = append(os.Environ(),
		"HINDSIGHT_LITE_LLM_BASE_URL="+env["HINDSIGHT_LITE_LLM_BASE_URL"],
		"HINDSIGHT_LITE_LLM_API_KEY="+env["HINDSIGHT_LITE_LLM_API_KEY"],
		"HINDSIGHT_LITE_LLM_MODEL="+env["HINDSIGHT_LITE_LLM_MODEL"],
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Println("start:", err)
		os.Exit(1)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	time.Sleep(800 * time.Millisecond)

	steps := []struct {
		note   string
		method string
		path   string
		body   any
	}{
		{"health", "GET", "/health", nil},
		{"PUT bank", "PUT", "/v1/default/banks/e2e", map[string]any{}},
		{"retain", "POST", "/v1/default/banks/e2e/memories", map[string]any{
			"items": []any{
				map[string]any{"content": "用户Alice住在柏林，2023年搬过去的。"},
				map[string]any{"content": "Alice养了一只猫叫Rex，Rex今年3岁。"},
			},
		}},
		{"recall", "POST", "/v1/default/banks/e2e/memories/recall", map[string]any{"query": "Alice 住在哪里"}},
		{"consolidate", "POST", "/v1/default/banks/e2e/consolidate", nil},
		{"recall-observations", "POST", "/v1/default/banks/e2e/memories/recall", map[string]any{
			"query": "Alice 住哪里", "types": []string{"observation"},
		}},
		{"reflect", "POST", "/v1/default/banks/e2e/reflect", map[string]any{
			"query":   "用户住在哪里？她养了什么宠物？",
			"include": map[string]any{"facts": map[string]any{}},
		}},
		{"create-mm", "POST", "/v1/default/banks/e2e/mental-models", map[string]any{
			"name": "user-profile", "source_query": "用户住在哪里？养了什么宠物？",
		}},
	}

	for _, s := range steps {
		var rdr *bytes.Reader
		if s.body != nil {
			raw, _ := json.Marshal(s.body)
			rdr = bytes.NewReader(raw)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(s.method, base+s.path, rdr)
		if err != nil {
			fmt.Println(s.note, "req:", err)
			continue
		}
		if s.body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		client := &http.Client{Timeout: 180 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("%-22s ERR: %v\n", s.note, err)
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		pretty := raw
		var buf bytes.Buffer
		if json.Indent(&buf, raw, "", " ") == nil {
			pretty = buf.Bytes()
		}
		out := string(pretty)
		if len(out) > 1200 {
			out = out[:1200] + " ...[truncated]"
		}
		fmt.Printf("\n=== %s -> %d ===\n%s\n", s.note, resp.StatusCode, out)
	}
}
