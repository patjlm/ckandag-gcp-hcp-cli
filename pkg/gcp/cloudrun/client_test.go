package cloudrun

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestParseResponse_Success(t *testing.T) {
	raw := `{
		"status": "success",
		"diagnosis": {
			"root_cause": "Pod etcd-0 is crashlooping due to disk pressure",
			"confidence": "high",
			"evidence": ["OOMKilled in events", "Disk usage at 95%"],
			"recommendation": "Increase PVC size to 20Gi",
			"severity": "critical"
		},
		"metadata": {
			"iterations": 3,
			"steps": ["Checked pod status", "Analyzed logs"]
		}
	}`

	resp, err := ParseResponse([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("expected status 'success', got %q", resp.Status)
	}
	if resp.Diagnosis.RootCause != "Pod etcd-0 is crashlooping due to disk pressure" {
		t.Errorf("unexpected root_cause: %q", resp.Diagnosis.RootCause)
	}
	if resp.Diagnosis.Confidence != "high" {
		t.Errorf("unexpected confidence: %q", resp.Diagnosis.Confidence)
	}
	if resp.Diagnosis.Severity != "critical" {
		t.Errorf("unexpected severity: %q", resp.Diagnosis.Severity)
	}
	if len(resp.Diagnosis.Evidence) != 2 {
		t.Errorf("expected 2 evidence items, got %d", len(resp.Diagnosis.Evidence))
	}
	if resp.Diagnosis.Recommendation != "Increase PVC size to 20Gi" {
		t.Errorf("unexpected recommendation: %q", resp.Diagnosis.Recommendation)
	}
}

func TestParseResponse_Error(t *testing.T) {
	raw := `{
		"status": "error",
		"error": "failed to connect to cluster",
		"diagnosis": {}
	}`

	resp, err := ParseResponse([]byte(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "error" {
		t.Errorf("expected status 'error', got %q", resp.Status)
	}
	if resp.Error != "failed to connect to cluster" {
		t.Errorf("expected error message, got %q", resp.Error)
	}
}

func TestParseResponse_InvalidJSON(t *testing.T) {
	_, err := ParseResponse([]byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestDiagnose_HTTPFlow(t *testing.T) {
	expectedResp := DiagnoseResponse{
		Status: "success",
		Diagnosis: Diagnosis{
			RootCause:      "OOM kill",
			Confidence:     "high",
			Evidence:       []string{"memory limit exceeded"},
			Recommendation: "increase memory",
			Severity:       "high",
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/diagnose" {
			t.Errorf("expected path /diagnose, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}

		var req DiagnoseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}
		if req.Query != "why is pod X failing" {
			t.Errorf("unexpected query: %q", req.Query)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expectedResp)
	}))
	defer server.Close()

	client := &Client{
		Project:    "test-project",
		Region:     "us-central1",
		httpClient: server.Client(),
	}

	resp, err := client.Diagnose(t.Context(), server.URL, "why is pod X failing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("expected status 'success', got %q", resp.Status)
	}
	if resp.Diagnosis.RootCause != "OOM kill" {
		t.Errorf("unexpected root_cause: %q", resp.Diagnosis.RootCause)
	}
}

func TestDiagnose_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer server.Close()

	client := &Client{
		Project:    "test-project",
		Region:     "us-central1",
		httpClient: server.Client(),
	}

	_, err := client.Diagnose(t.Context(), server.URL, "test query")
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestDiagnoseStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req DiagnoseRequest
		json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Error("expected stream=true in request")
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)

		// Send tool_call event
		fmt.Fprintln(w, `{"event":"tool_call","tool":"get_resources","parameters":{"resource_type":"pods","namespace":"default"}}`)
		flusher.Flush()

		// Send tool_result event
		fmt.Fprintln(w, `{"event":"tool_result","tool":"get_resources","result":"PodList: 3 items"}`)
		flusher.Flush()

		// Send done event with final result
		result := DiagnoseResponse{
			Status: "success",
			Diagnosis: Diagnosis{
				RootCause:      "Pod is OOMKilled",
				Confidence:     "high",
				Evidence:       []string{"memory exceeded"},
				Recommendation: "increase limits",
				Severity:       "high",
			},
		}
		resultJSON, _ := json.Marshal(result)
		fmt.Fprintf(w, `{"event":"done","result":%s}`, string(resultJSON))
		fmt.Fprintln(w)
		flusher.Flush()
	}))
	defer server.Close()

	client := &Client{
		Project:    "test-project",
		Region:     "us-central1",
		httpClient: server.Client(),
	}

	var events []StreamEvent
	resp, err := client.DiagnoseStream(t.Context(), server.URL, "test query", func(event StreamEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].Event != "tool_call" || events[0].Tool != "get_resources" {
		t.Errorf("unexpected first event: %+v", events[0])
	}
	if events[1].Event != "tool_result" {
		t.Errorf("unexpected second event: %+v", events[1])
	}
	if events[2].Event != "done" {
		t.Errorf("unexpected third event: %+v", events[2])
	}

	if resp.Status != "success" {
		t.Errorf("expected status 'success', got %q", resp.Status)
	}
	if resp.Diagnosis.RootCause != "Pod is OOMKilled" {
		t.Errorf("unexpected root_cause: %q", resp.Diagnosis.RootCause)
	}
}

func TestDiagnoseStream_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"event":"tool_call","tool":"get_resources","parameters":{}}`)
		fmt.Fprintln(w, `{"event":"error","error":"cluster unreachable"}`)
	}))
	defer server.Close()

	client := &Client{
		Project:    "test-project",
		Region:     "us-central1",
		httpClient: server.Client(),
	}

	_, err := client.DiagnoseStream(t.Context(), server.URL, "test", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "cluster unreachable") {
		t.Errorf("expected error to contain 'cluster unreachable', got %q", err.Error())
	}
}

func TestWrapAuthError(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantMsg string
	}{
		{
			name:    "credentials not found",
			err:     fmt.Errorf("could not find default credentials"),
			wantMsg: "no GCP credentials found",
		},
		{
			name:    "permission denied",
			err:     fmt.Errorf("PermissionDenied"),
			wantMsg: "permission denied",
		},
		{
			name:    "not found",
			err:     fmt.Errorf("NotFound"),
			wantMsg: "service not found",
		},
		{
			name:    "generic error",
			err:     fmt.Errorf("something else"),
			wantMsg: "something else",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := wrapAuthError("testing", tt.err)
			if wrapped == nil {
				t.Fatal("expected non-nil error")
			}
			if !contains(wrapped.Error(), tt.wantMsg) {
				t.Errorf("expected error to contain %q, got %q", tt.wantMsg, wrapped.Error())
			}
		})
	}
}

func TestDiagnose_RetryOn503ThenSuccess(t *testing.T) {
	var attempts atomic.Int32
	expectedResp := DiagnoseResponse{
		Status: "success",
		Diagnosis: Diagnosis{
			RootCause:      "cold start resolved",
			Confidence:     "high",
			Evidence:       []string{"service warmed up"},
			Recommendation: "none",
			Severity:       "low",
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("service unavailable"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expectedResp)
	}))
	defer server.Close()

	client := &Client{
		Project:    "test-project",
		Region:     "us-central1",
		httpClient: server.Client(),
	}

	resp, err := client.Diagnose(t.Context(), server.URL, "test cold start")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "success" {
		t.Errorf("expected status 'success', got %q", resp.Status)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("expected 2 attempts, got %d", got)
	}
}

func TestDiagnose_GivesUpAfterMaxRetries(t *testing.T) {
	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("always unavailable"))
	}))
	defer server.Close()

	client := &Client{
		Project:    "test-project",
		Region:     "us-central1",
		httpClient: server.Client(),
	}

	_, err := client.Diagnose(t.Context(), server.URL, "test max retries")
	if err == nil {
		t.Fatal("expected error after max retries")
	}
	if !contains(err.Error(), "503") {
		t.Errorf("expected error to mention 503, got %q", err.Error())
	}
	// 1 initial + 3 retries = 4 total
	if got := attempts.Load(); got != 4 {
		t.Errorf("expected 4 attempts, got %d", got)
	}
}

func TestDiagnose_NoRetryOnNonTransient(t *testing.T) {
	for _, code := range []int{400, 401, 404} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			var attempts atomic.Int32

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(code)
				w.Write([]byte("non-transient error"))
			}))
			defer server.Close()

			client := &Client{
				Project:    "test-project",
				Region:     "us-central1",
				httpClient: server.Client(),
			}

			_, err := client.Diagnose(t.Context(), server.URL, "test no retry")
			if err == nil {
				t.Fatal("expected error")
			}
			if got := attempts.Load(); got != 1 {
				t.Errorf("expected 1 attempt (no retry), got %d", got)
			}
		})
	}
}

func TestChatStream_TextAndDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat" {
			t.Errorf("expected path /chat, got %s", r.URL.Path)
		}

		var req ChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.History) != 1 || req.History[0].Content != "hello" {
			t.Errorf("unexpected history: %+v", req.History)
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"event":"text","content":"Hi there!"}`)
		fmt.Fprintln(w, `{"event":"done"}`)
	}))
	defer server.Close()

	client := &Client{
		Project:    "test-project",
		Region:     "us-central1",
		httpClient: server.Client(),
	}

	var events []StreamEvent
	result, err := client.ChatStream(t.Context(), server.URL, ChatRequest{
		History: []ChatMessage{{Role: "user", Content: "hello"}},
	}, func(event StreamEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Event != "text" || events[0].Content != "Hi there!" {
		t.Errorf("unexpected first event: %+v", events[0])
	}
	if events[1].Event != "done" {
		t.Errorf("unexpected second event: %+v", events[1])
	}
	if result.PendingToolCall != nil {
		t.Error("expected no pending tool call")
	}
}

func TestChatStream_ToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"event":"text","content":"Let me delete that pod."}`)
		fmt.Fprintln(w, `{"event":"tool_call","call_id":"tc-1","tool":"wf_run_delete_pod","parameters":{"namespace":"default","pod":"test-pod"}}`)
	}))
	defer server.Close()

	client := &Client{
		Project:    "test-project",
		Region:     "us-central1",
		httpClient: server.Client(),
	}

	var events []StreamEvent
	result, err := client.ChatStream(t.Context(), server.URL, ChatRequest{
		History: []ChatMessage{{Role: "user", Content: "delete pod test-pod"}},
	}, func(event StreamEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if result.PendingToolCall == nil {
		t.Fatal("expected pending tool call")
	}
	if result.PendingToolCall.Tool != "wf_run_delete_pod" {
		t.Errorf("unexpected tool: %s", result.PendingToolCall.Tool)
	}
	if result.PendingToolCall.CallID != "tc-1" {
		t.Errorf("unexpected call_id: %s", result.PendingToolCall.CallID)
	}
}

func TestChatStream_ToolResult(t *testing.T) {
	// Test that tool_result in request is properly sent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.ToolResult == nil {
			t.Fatal("expected tool_result in request")
		}
		if req.ToolResult.CallID != "tc-1" {
			t.Errorf("unexpected call_id: %s", req.ToolResult.CallID)
		}
		if req.ToolResult.Tool != "wf_run_delete_pod" {
			t.Errorf("unexpected tool: %s", req.ToolResult.Tool)
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"event":"text","content":"Pod deleted successfully."}`)
		fmt.Fprintln(w, `{"event":"done"}`)
	}))
	defer server.Close()

	client := &Client{
		Project:    "test-project",
		Region:     "us-central1",
		httpClient: server.Client(),
	}

	result, err := client.ChatStream(t.Context(), server.URL, ChatRequest{
		History: []ChatMessage{{Role: "user", Content: "delete pod"}},
		ToolResult: &ChatToolResult{
			CallID: "tc-1",
			Tool:   "wf_run_delete_pod",
			Result: json.RawMessage(`{"state":"SUCCEEDED"}`),
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.PendingToolCall != nil {
		t.Error("expected no pending tool call")
	}
}

func TestChatStream_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"event":"error","error":"model overloaded"}`)
	}))
	defer server.Close()

	client := &Client{
		Project:    "test-project",
		Region:     "us-central1",
		httpClient: server.Client(),
	}

	_, err := client.ChatStream(t.Context(), server.URL, ChatRequest{
		History: []ChatMessage{{Role: "user", Content: "test"}},
	}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "model overloaded") {
		t.Errorf("expected error to contain 'model overloaded', got %q", err.Error())
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
