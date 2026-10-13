package oneb_gen

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DawnKosmos/oneb"
)

type nested struct {
	ID string `json:"id"`
}
type input struct {
	Left  float64 `json:"left"`
	Right float64 `json:"right"`
}
type output struct {
	Sum     float64        `json:"sum"`
	Next    *output        `json:"next,omitempty"`
	Items   []nested       `json:"items"`
	Data    []byte         `json:"data"`
	Created time.Time      `json:"created"`
	ByID    map[int]nested `json:"by_id"`
	Count   int64          `json:"count,string"`
	Hidden  string         `json:"-"`
}

func routes(router *oneb.Router) {
	router.Group("math").Add("add", func(context.Context, input) (output, error) {
		panic("generation executed a handler")
	}, oneb.WithPositionalParams())
}

func read(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestGenerate(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(routes, dir); err != nil {
		t.Fatal(err)
	}
	models := read(t, filepath.Join(dir, "models.ts"))
	client := read(t, filepath.Join(dir, "client.ts"))
	for _, want := range []string{
		`export type input =`, `"left": number;`, `export type nested =`,
		`"next"?: (output) | null;`, `"items": Array<nested> | null;`,
		`"data": string | null;`, `"created": string;`,
		`"by_id": Record<string, nested> | null;`, `"count": string;`,
	} {
		if !strings.Contains(models, want) {
			t.Errorf("models missing %q:\n%s", want, models)
		}
	}
	if strings.Contains(models, "Hidden") {
		t.Fatal("ignored field was generated")
	}
	for _, want := range []string{
		`["math/add"]: (params: Models.input | [number, number]): Promise<Models.output>`,
		`call<Models.output>("math/add", params)`, `method: "POST"`,
		`jsonrpc: "2.0"`, `new Headers(options.headers)`, `export class RpcError`,
	} {
		if !strings.Contains(client, want) {
			t.Errorf("client missing %q:\n%s", want, client)
		}
	}
	if err := Generate(routes, dir); err != nil {
		t.Fatal(err)
	}
	if models != read(t, filepath.Join(dir, "models.ts")) || client != read(t, filepath.Join(dir, "client.ts")) {
		t.Fatal("generation is not deterministic")
	}
}

func TestGenerateErrorsBeforeWriting(t *testing.T) {
	tests := []struct {
		name     string
		register func(*oneb.Router)
	}{
		{"nil", nil},
		{"empty", func(*oneb.Router) {}},
		{"unsupported field", func(router *oneb.Router) {
			router.Add("bad", func(context.Context, input) (chan int, error) { return nil, nil })
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "generated")
			if err := Generate(test.register, dir); err == nil {
				t.Fatal("expected error")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("failed generation wrote output: %v", err)
			}
		})
	}
	if err := Generate(routes, " "); err == nil {
		t.Fatal("accepted empty output directory")
	}
}

type Embedded struct {
	Value  string `json:"value"`
	Hidden int    `json:"-"`
}
type embeddedModel struct {
	*Embedded
	Value    int  `json:"value"`
	Optional bool `json:"optional,omitzero"`
}

func TestJSONFieldPromotion(t *testing.T) {
	g := newModels()
	if _, err := g.typeOf(reflect.TypeFor[embeddedModel]()); err != nil {
		t.Fatal(err)
	}
	source := g.source()
	if !strings.Contains(source, `"value": number;`) || !strings.Contains(source, `"optional"?: boolean;`) || strings.Contains(source, "Hidden") {
		t.Fatalf("incorrect promoted fields:\n%s", source)
	}
}

type customJSON struct{}

func (customJSON) MarshalJSON() ([]byte, error) { return []byte(`"custom"`), nil }

func TestRejectCustomJSONShape(t *testing.T) {
	if _, err := newModels().typeOf(reflect.TypeFor[customJSON]()); err == nil {
		t.Fatal("custom marshaler silently generated an incorrect shape")
	}
}

func TestInlineModelsAndPrototypeMethod(t *testing.T) {
	dir := t.TempDir()
	if err := Generate(func(router *oneb.Router) {
		router.Add("__proto__", func(context.Context, struct{ Value string }) (bool, error) { return true, nil })
	}, dir); err != nil {
		t.Fatal(err)
	}
	client := read(t, filepath.Join(dir, "client.ts"))
	if strings.Contains(client, "import type") || !strings.Contains(client, `["__proto__"]:`) {
		t.Fatalf("invalid inline client:\n%s", client)
	}
}

type class struct {
	Count *int `json:"count,string"`
}

func TestReservedModelNameAndQuotedPositionalField(t *testing.T) {
	g := newModels()
	name, err := g.typeOf(reflect.TypeFor[class]())
	if err != nil || name != "_class" {
		t.Fatalf("reserved name: %q, %v", name, err)
	}
	tuple, ok, err := g.positional(reflect.TypeFor[class]())
	if err != nil || !ok || tuple != "[string | null]" {
		t.Fatalf("quoted positional field: %q, %v", tuple, err)
	}
}
