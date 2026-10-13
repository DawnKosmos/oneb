package oneb_gen

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type modelGenerator struct {
	definitions map[string]string
	owners      map[string]reflect.Type
	namespace   string
}

func newModels() *modelGenerator {
	return &modelGenerator{definitions: make(map[string]string), owners: make(map[string]reflect.Type)}
}

var (
	identifierChars = regexp.MustCompile(`[^a-zA-Z0-9_$]`)
	jsonMarshaler   = reflect.TypeFor[json.Marshaler]()
	textMarshaler   = reflect.TypeFor[encoding.TextMarshaler]()
)

func (g *modelGenerator) typeOf(t reflect.Type) (string, error) {
	if t == nil {
		return "", fmt.Errorf("missing Go type")
	}
	if t.Kind() == reflect.Pointer {
		inner, err := g.typeOf(t.Elem())
		return "(" + inner + ") | null", err
	}
	if t == reflect.TypeFor[json.RawMessage]() {
		return "unknown", nil
	}
	if t == reflect.TypeFor[time.Time]() {
		return "string", nil
	}
	if t.Implements(jsonMarshaler) || reflect.PointerTo(t).Implements(jsonMarshaler) {
		return "", fmt.Errorf("custom JSON marshaler %s requires an explicit wire contract", t)
	}
	if t.Implements(textMarshaler) || reflect.PointerTo(t).Implements(textMarshaler) {
		return "string", nil
	}
	if t.Name() == "" || t.PkgPath() == "" {
		return g.underlying(t)
	}
	name := identifierChars.ReplaceAllString(t.Name(), "_")
	name = strings.TrimRight(name, "_")
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		name = "_" + name
	}
	if slices.Contains(strings.Fields("break case catch class const continue debugger default delete do else enum export extends false finally for function if import in instanceof new null return super switch this throw true try typeof var void while with implements interface let package private protected public static yield any boolean number string symbol unknown never object undefined await"), name) {
		name = "_" + name
	}
	if previous, exists := g.owners[name]; exists {
		if previous != t {
			return "", fmt.Errorf("TypeScript name %q collides between %s and %s", name, previous, t)
		}
		return g.namespace + name, nil
	}
	g.owners[name] = t // Register before visiting fields to support recursive models.
	body, err := g.underlying(t)
	if err != nil {
		return "", err
	}
	g.definitions[name] = "export type " + name + " = " + body + ";\n"
	return g.namespace + name, nil
}

func (g *modelGenerator) underlying(t reflect.Type) (string, error) {
	switch t.Kind() {
	case reflect.String:
		return "string", nil
	case reflect.Bool:
		return "boolean", nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64:
		return "number", nil
	case reflect.Interface:
		return "unknown", nil
	case reflect.Slice, reflect.Array:
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return "string | null", nil // encoding/json uses base64 for []byte.
		}
		inner, err := g.typeOf(t.Elem())
		if err != nil {
			return "", err
		}
		result := "Array<" + inner + ">"
		if t.Kind() == reflect.Slice {
			result += " | null"
		}
		return result, nil
	case reflect.Map:
		key := t.Key()
		if key.Kind() != reflect.String && !isInteger(key.Kind()) && !key.Implements(textMarshaler) {
			return "", fmt.Errorf("unsupported JSON map key %s", key)
		}
		value, err := g.typeOf(t.Elem())
		return "Record<string, " + value + "> | null", err
	case reflect.Struct:
		fields := jsonFields(t)
		var body strings.Builder
		body.WriteString("{\n")
		for _, field := range fields {
			value, err := g.typeOf(field.typ)
			if err != nil {
				return "", fmt.Errorf("%s.%s: %w", t, field.name, err)
			}
			if field.quoted && stringEncodable(field.typ) {
				value = "string"
				if field.typ.Kind() == reflect.Pointer {
					value += " | null"
				}
			}
			optional := ""
			if field.optional {
				optional = "?"
			}
			fmt.Fprintf(&body, "  %s%s: %s;\n", strconv.Quote(field.name), optional, value)
		}
		body.WriteString("}")
		return body.String(), nil
	default:
		return "", fmt.Errorf("unsupported JSON type %s", t)
	}
}

func isInteger(kind reflect.Kind) bool {
	return kind >= reflect.Int && kind <= reflect.Uintptr
}

func stringEncodable(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return isInteger(t.Kind()) || t.Kind() == reflect.String || t.Kind() == reflect.Bool || t.Kind() == reflect.Float32 || t.Kind() == reflect.Float64
}

type jsonField struct {
	name                     string
	typ                      reflect.Type
	depth                    int
	tagged, optional, quoted bool
}

// Follow encoding/json's promotion and dominance rules for embedded fields.
func jsonFields(t reflect.Type) []jsonField {
	var candidates []jsonField
	var visit func(reflect.Type, int, bool, map[reflect.Type]bool)
	visit = func(t reflect.Type, depth int, optional bool, parents map[reflect.Type]bool) {
		if parents[t] {
			return
		}
		parents[t] = true
		defer delete(parents, t)
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			parts := strings.Split(field.Tag.Get("json"), ",")
			if parts[0] == "-" {
				continue
			}
			base := field.Type
			if base.Kind() == reflect.Pointer {
				base = base.Elem()
			}
			if !field.IsExported() && (!field.Anonymous || base.Kind() != reflect.Struct) {
				continue
			}
			if field.Anonymous && parts[0] == "" && base.Kind() == reflect.Struct {
				visit(base, depth+1, optional || field.Type.Kind() == reflect.Pointer, parents)
				continue
			}
			name := parts[0]
			if name == "" {
				name = field.Name
			}
			candidates = append(candidates, jsonField{
				name: name, typ: field.Type, depth: depth, tagged: parts[0] != "",
				optional: optional || slices.Contains(parts[1:], "omitempty") || slices.Contains(parts[1:], "omitzero"),
				quoted:   slices.Contains(parts[1:], "string"),
			})
		}
	}
	visit(t, 0, false, make(map[reflect.Type]bool))
	slices.SortFunc(candidates, func(a, b jsonField) int {
		if result := strings.Compare(a.name, b.name); result != 0 {
			return result
		}
		if a.depth != b.depth {
			return a.depth - b.depth
		}
		if a.tagged != b.tagged {
			if a.tagged {
				return -1
			}
			return 1
		}
		return 0
	})
	var fields []jsonField
	for i := 0; i < len(candidates); {
		j := i + 1
		for j < len(candidates) && candidates[j].name == candidates[i].name {
			j++
		}
		if j == i+1 || candidates[i].depth != candidates[i+1].depth || candidates[i].tagged != candidates[i+1].tagged {
			fields = append(fields, candidates[i])
		}
		i = j
	}
	return fields
}

func (g *modelGenerator) positional(t reflect.Type) (string, bool, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return "", false, nil
	}
	var values []string
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := strings.Split(field.Tag.Get("json"), ",")[0]
		if !field.IsExported() || tag == "-" || field.Anonymous && tag == "" {
			continue
		}
		value, err := g.typeOf(field.Type)
		if err != nil {
			return "", false, err
		}
		if slices.Contains(strings.Split(field.Tag.Get("json"), ",")[1:], "string") && stringEncodable(field.Type) {
			value = "string"
			if field.Type.Kind() == reflect.Pointer {
				value += " | null"
			}
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return "", false, nil
	}
	return "[" + strings.Join(values, ", ") + "]", true, nil
}

func (g *modelGenerator) source() string {
	names := make([]string, 0, len(g.definitions))
	for name := range g.definitions {
		names = append(names, name)
	}
	slices.Sort(names)
	var source strings.Builder
	source.WriteString("// Code generated by oneb_gen. DO NOT EDIT.\n\n")
	for _, name := range names {
		source.WriteString(g.definitions[name])
		source.WriteByte('\n')
	}
	if len(names) == 0 {
		source.WriteString("export {};\n")
	}
	return source.String()
}
