package oneb_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DawnKosmos/oneb"
	"github.com/creachadair/jrpc2"
)

type arithmeticInput struct {
	Left  float64 `json:"left"`
	Right float64 `json:"right"`
}

type arithmeticOutput struct {
	Sum float64 `json:"sum"`
}

func add(_ context.Context, input arithmeticInput) (arithmeticOutput, error) {
	return arithmeticOutput{Sum: input.Left + input.Right}, nil
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *jrpc2.Error    `json:"error"`
}

func newRouter(t *testing.T) *oneb.Router {
	t.Helper()
	router := oneb.New()
	t.Cleanup(func() {
		if err := router.Close(); err != nil {
			t.Errorf("closing router: %v", err)
		}
	})
	return router
}

func post(router http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/rpc", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func TestTypedMethodsAndGroups(t *testing.T) {
	router := newRouter(t)
	router.Add("echo", func(_ context.Context, input map[string]string) (map[string]string, error) {
		return input, nil
	})
	math := router.Group("math")
	math.Add[arithmeticInput, arithmeticOutput]("add", add, oneb.WithStrictParams())
	math.Add("positional", add, oneb.WithPositionalParams(), oneb.WithStrictParams())
	math.Add("relaxed", add)
	math.Group("v1").Add("add", add)
	router.Group("other").Add("add", func(_ context.Context, input arithmeticInput) (arithmeticOutput, error) {
		return arithmeticOutput{Sum: input.Left + input.Right + 100}, nil
	})
	router.Add("context", func(ctx context.Context, _ struct{}) (string, error) {
		return jrpc2.InboundRequest(ctx).Method(), nil
	})
	math.Add[struct{}, string]("context", func(ctx context.Context, _ struct{}) (string, error) {
		return jrpc2.InboundRequest(ctx).Method(), nil
	})
	router.Add("failure", func(_ context.Context, _ struct{}) (any, error) {
		return "discarded", jrpc2.Errorf(1001, "business rule failed")
	})
	router.Add("null", func(_ context.Context, _ struct{}) (any, error) { return nil, nil })

	tests := []struct {
		name   string
		method string
		params string
		result string
		code   jrpc2.Code
	}{
		{name: "root", method: "echo", params: `{"message":"hello"}`, result: `{"message":"hello"}`},
		{name: "group", method: "math/add", params: `{"left":2,"right":3}`, result: `{"sum":5}`},
		{name: "nested group", method: "math/v1/add", params: `{"left":2,"right":3}`, result: `{"sum":5}`},
		{name: "separate group", method: "other/add", params: `{"left":2,"right":3}`, result: `{"sum":105}`},
		{name: "group prefix required", method: "add", params: `{}`, code: jrpc2.MethodNotFound},
		{name: "unknown method", method: "missing", params: `{}`, code: jrpc2.MethodNotFound},
		{name: "wrong field type", method: "math/add", params: `{"left":"two","right":3}`, code: jrpc2.InvalidParams},
		{name: "unknown field", method: "math/add", params: `{"left":2,"right":3,"extra":4}`, code: jrpc2.InvalidParams},
		{name: "relaxed fields", method: "math/relaxed", params: `{"left":2,"right":3,"extra":4}`, result: `{"sum":5}`},
		{name: "object default", method: "math/add", params: `[2,3]`, code: jrpc2.InvalidParams},
		{name: "positional opt in", method: "math/positional", params: `[2,3]`, result: `{"sum":5}`},
		{name: "positional count", method: "math/positional", params: `[2]`, code: jrpc2.InvalidParams},
		{name: "positional also accepts object", method: "math/positional", params: `{"left":2,"right":3}`, result: `{"sum":5}`},
		{name: "context handler", method: "context", params: `{}`, result: `"context"`},
		{name: "group context handler", method: "math/context", params: `{}`, result: `"math/context"`},
		{name: "handler error", method: "failure", params: `{}`, code: 1001},
		{name: "null result", method: "null", params: `{}`, result: `null`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := post(router, fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s,"id":"request"}`, test.method, test.params))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
			}
			var response rpcResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.JSONRPC != "2.0" || string(response.ID) != `"request"` {
				t.Fatalf("invalid response envelope: %s", recorder.Body)
			}
			if test.code != 0 {
				if response.Error == nil || response.Error.Code != test.code || len(response.Result) != 0 {
					t.Fatalf("want error %d without result, got %s", test.code, recorder.Body)
				}
			} else if response.Error != nil || string(response.Result) != test.result {
				t.Fatalf("want result %s, got %s", test.result, recorder.Body)
			}
		})
	}
}

func TestNotificationsAndBatches(t *testing.T) {
	router := newRouter(t)
	router.Group("math").Add("add", add)
	notified := make(chan struct{}, 2)
	router.Add("notify", func(_ context.Context, _ struct{}) (any, error) {
		notified <- struct{}{}
		return nil, nil
	})

	recorder := post(router, `{"jsonrpc":"2.0","method":"notify","params":{}}`)
	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 {
		t.Fatalf("notification: status = %d, body = %s", recorder.Code, recorder.Body)
	}
	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("notification handler did not run")
	}

	recorder = post(router, `[
		{"jsonrpc":"2.0","method":"math/add","params":{"left":2,"right":3},"id":1},
		{"jsonrpc":"2.0","method":"notify","params":{}},
		{"jsonrpc":"2.0","method":"missing","id":2}
	]`)
	var responses []rpcResponse
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &responses) != nil || len(responses) != 2 {
		t.Fatalf("batch: status = %d, body = %s", recorder.Code, recorder.Body)
	}
	byID := make(map[string]rpcResponse)
	for _, response := range responses {
		byID[string(response.ID)] = response
	}
	if response := byID["1"]; response.Error != nil || string(response.Result) != `{"sum":5}` {
		t.Fatalf("batch result: %+v", response)
	}
	if response := byID["2"]; response.Error == nil || response.Error.Code != jrpc2.MethodNotFound {
		t.Fatalf("batch error: %+v", response)
	}
	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("batch notification handler did not run")
	}
}

func TestHTTPRouter(t *testing.T) {
	router := newRouter(t)
	router.Group("math").Add("add", add)
	mux := http.NewServeMux()
	mux.Handle("/rpc", router)
	server := httptest.NewServer(mux)
	defer server.Close()

	response, err := http.Post(server.URL+"/rpc", "application/json", strings.NewReader(`{"jsonrpc":"2.0","method":"math/add","params":{"left":2,"right":3},"id":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result rpcResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(result.Result) != `{"sum":5}` {
		t.Fatalf("status = %d, result = %+v", response.StatusCode, result)
	}

	for _, test := range []struct {
		method      string
		contentType string
		status      int
	}{
		{method: http.MethodGet, status: http.StatusMethodNotAllowed},
		{method: http.MethodPost, contentType: "text/plain", status: http.StatusUnsupportedMediaType},
	} {
		req := httptest.NewRequest(test.method, "/rpc", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", test.contentType)
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, req)
		if recorder.Code != test.status {
			t.Errorf("%s %s: status = %d, want %d", test.method, test.contentType, recorder.Code, test.status)
		}
	}
}

func TestInvalidRegistration(t *testing.T) {
	tests := []struct {
		name     string
		register func(*oneb.Router)
	}{
		{name: "empty", register: func(r *oneb.Router) { r.Add("", add) }},
		{name: "reserved", register: func(r *oneb.Router) { r.Add("rpc.custom", add) }},
		{name: "empty group", register: func(r *oneb.Router) { r.Group("") }},
		{name: "nil handler", register: func(r *oneb.Router) { r.Add[arithmeticInput, arithmeticOutput]("add", nil) }},
		{name: "duplicate", register: func(r *oneb.Router) {
			r.Group("math").Add("add", add)
			r.Group("math").Add("add", add)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router := newRouter(t)
			defer func() {
				if recover() == nil {
					t.Fatal("invalid registration did not panic")
				}
			}()
			test.register(router)
		})
	}
}

func TestConcurrentRegistrationAndDispatch(t *testing.T) {
	router := newRouter(t)
	math := router.Group("math")
	math.Add("add", add)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			math.Add(fmt.Sprintf("add%d", i), add)
			recorder := post(router, `{"jsonrpc":"2.0","method":"math/add","params":{"left":2,"right":3},"id":1}`)
			var response rpcResponse
			if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &response) != nil || string(response.Result) != `{"sum":5}` {
				t.Errorf("concurrent call: status = %d, body = %s", recorder.Code, recorder.Body)
			}
		})
	}
	wg.Wait()
}
