package config

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// The shapes of the config's free-form parts (the loader reads them as raw JSON), so their keys can be checked too.
type (
	trackerLabels struct {
		Trigger json.RawMessage `json:"trigger"`
	}
	trackerTransitions struct {
		OnStart   json.RawMessage `json:"onStart"`
		OnSuccess json.RawMessage `json:"onSuccess"`
		OnFailure json.RawMessage `json:"onFailure"`
		OnMerged  json.RawMessage `json:"onMerged"`
	}
	// projectShape: a project is either {default, repos} or, in the short form, its one repo's keys.
	projectShape struct {
		rawProject
		rawRepo
	}
)

// rawOverrides: the type of a json.RawMessage (or map of them) field, by "struct.key".
var rawOverrides = map[string]reflect.Type{
	"rawTracker.labels":      reflect.TypeFor[trackerLabels](),
	"rawTracker.transitions": reflect.TypeFor[trackerTransitions](),
	"rawTracker.projects":    reflect.TypeFor[map[string]projectShape](),
}

// UnknownKeys lists the keys of a mako.config.json the loader does not read ("executors.x.args"), sorted, a list item not named in the path. The
// loader ignores them silently, so a misspelt or removed setting does nothing. Not a JSON object: nothing.
func UnknownKeys(data []byte) []string { return checkKeys(data).unknown }

// checkKeys walks the config: the keys it does not read (sorted) and the ones it does.
func checkKeys(data []byte) *keyWalk {
	w := &keyWalk{seen: map[string]bool{}}
	var v any
	if json.Unmarshal(data, &v) == nil {
		w.walk("", "", v, reflect.TypeFor[rawConfig]())
	}
	sort.Strings(w.unknown)
	return w
}

type keyWalk struct {
	unknown []string
	seen    map[string]bool // the keys met, by their path with every map key as "*" and every list item as "[]"
}

type keyField struct {
	name string
	typ  reflect.Type
}

// fields: the json keys of a struct, the embedded structs' included, with the type of each. A key with no tag is
// its field's name; the decoder matches keys without regard to case, so do the callers.
func fields(t reflect.Type) []keyField {
	var out []keyField
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case name == "-":
			continue
		case f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct:
			out = append(out, fields(f.Type)...)
			continue
		case !f.IsExported():
			continue
		case name == "":
			name = f.Name
		}
		typ := f.Type
		if typ == reflect.TypeFor[json.RawMessage]() || typ == reflect.TypeFor[map[string]json.RawMessage]() {
			typ = rawOverrides[t.Name()+"."+name]
		}
		out = append(out, keyField{name, typ})
	}
	return out
}

func (w *keyWalk) walk(path, norm string, v any, t reflect.Type) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return
	}
	switch v := v.(type) {
	case map[string]any:
		switch t.Kind() {
		case reflect.Struct:
			known := fields(t)
			for key, val := range v {
				i := indexFold(known, key)
				if i < 0 {
					w.unknown = append(w.unknown, join(path, key))
					continue
				}
				w.seen[join(norm, known[i].name)] = true
				w.walk(join(path, key), join(norm, known[i].name), val, known[i].typ)
			}
		case reflect.Map:
			for key, val := range v {
				w.walk(join(path, key), join(norm, "*"), val, t.Elem())
			}
		}
	case []any:
		if t.Kind() == reflect.Slice {
			for _, val := range v {
				w.walk(path, norm+"[]", val, t.Elem())
			}
		}
	}
}

func indexFold(fs []keyField, key string) int {
	for i, f := range fs {
		if strings.EqualFold(f.name, key) {
			return i
		}
	}
	return -1
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// allKeys: every key path the loader reads under t, in the form keyWalk.seen has. A project is {default, repos}
// there: its short form is the same repo keys one level up.
func allKeys(norm string, t reflect.Type, out map[string]bool) {
	for t != nil && (t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map) {
		switch t.Kind() {
		case reflect.Slice:
			norm += "[]"
		case reflect.Map:
			norm = join(norm, "*")
		}
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return
	}
	if t == reflect.TypeFor[projectShape]() {
		t = reflect.TypeFor[rawProject]()
	}
	for _, f := range fields(t) {
		out[join(norm, f.name)] = true
		allKeys(join(norm, f.name), f.typ, out)
	}
}
