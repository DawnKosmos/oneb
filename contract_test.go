package oneb

import (
	"context"
	"reflect"
	"sync"
	"testing"
)

type captureInput struct {
	Value string `json:"value"`
}
type captureOutput struct {
	Result string `json:"result"`
}

func captureHandler(context.Context, captureInput) (captureOutput, error) {
	panic("generation must not execute handlers")
}

func TestCaptureRoutes(t *testing.T) {
	normal := New()
	defer normal.Close()
	normal.Add("normal", captureHandler)
	if normal.contracts != nil {
		t.Fatal("normal startup activated contract recording")
	}
	registry := CaptureRoutes(func(router *Router) {
		router.Group("math").Group("v1").Add("add", captureHandler, WithPositionalParams())
		router.Add("echo", captureHandler)
		normal.Add("unrelated", captureHandler)
	})
	operations := registry.Operations()
	if len(operations) != 2 || operations[0].Name != "echo" || operations[1].Name != "math/v1/add" {
		t.Fatalf("unexpected contracts: %+v", operations)
	}
	if operations[0].Input != reflect.TypeFor[captureInput]() || operations[0].Output != reflect.TypeFor[captureOutput]() || !operations[1].PositionalParams {
		t.Fatalf("lost type or option information: %+v", operations)
	}
	operations[0].Name = "changed"
	if registry.Operations()[0].Name != "echo" {
		t.Fatal("snapshot mutates registry")
	}
}

func TestConcurrentCaptures(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			operations := CaptureRoutes(func(router *Router) { router.Add("echo", captureHandler) }).Operations()
			if len(operations) != 1 || operations[0].Name != "echo" {
				t.Errorf("capture leaked: %+v", operations)
			}
		})
	}
	wg.Wait()
}
