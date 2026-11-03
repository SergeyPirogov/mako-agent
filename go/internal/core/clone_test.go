package core

import (
	"encoding/json"
	"reflect"
	"testing"
)

// fill sets every field under v to a non-zero value, slices and maps with one element, so a copy that skips a
// field shows.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.Ptr:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k)
		fill(e)
		v.SetMapIndex(k, e)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				fill(v.Field(i))
			}
		}
	}
}

// shared lists the paths where a and b hold the same pointer, slice or map.
func shared(path string, a, b reflect.Value) []string {
	var out []string
	switch a.Kind() {
	case reflect.Ptr:
		if a.IsNil() {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			out = append(out, path)
		}
		out = append(out, shared(path+"*", a.Elem(), b.Elem())...)
	case reflect.Slice:
		if a.Len() > 0 && a.Pointer() == b.Pointer() {
			out = append(out, path)
		}
		for i := 0; i < a.Len() && i < b.Len(); i++ {
			out = append(out, shared(path+"[]", a.Index(i), b.Index(i))...)
		}
	case reflect.Map:
		if a.IsNil() {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			out = append(out, path)
		}
		for _, k := range a.MapKeys() {
			if e := b.MapIndex(k); e.IsValid() {
				out = append(out, shared(path+"{}", a.MapIndex(k), e)...)
			}
		}
	case reflect.Struct:
		for i := 0; i < a.NumField(); i++ {
			out = append(out, shared(path+"."+a.Type().Field(i).Name, a.Field(i), b.Field(i))...)
		}
	}
	return out
}

// Every pointer, slice and map in a record is copied: the store's Get hands a clone to readers while Update
// changes the stored record. A new field of Record that Clone forgets fails here.
func TestRecordCloneIsDeep(t *testing.T) {
	var r Record
	fill(reflect.ValueOf(&r).Elem())
	c := r.Clone()
	if !reflect.DeepEqual(r, c) {
		t.Fatalf("the clone differs from the record:\n%+v\n%+v", r, c)
	}
	if paths := shared("Record", reflect.ValueOf(r), reflect.ValueOf(c)); len(paths) > 0 {
		t.Fatalf("the clone shares memory with the record at: %v", paths)
	}
}

// Changing what the clone points to leaves the record as it was.
func TestRecordCloneIsIndependent(t *testing.T) {
	r := Record{ID: "a", Task: Task{Comments: []Comment{{Author: "me", Body: "one"}}}, Checks: &Checks{Error: "old"}}
	c := r.Clone()
	c.Checks.Error = "new"
	c.Task.Comments[0].Body = "changed"
	c.Task.Comments = append(c.Task.Comments, Comment{Body: "two"})
	if r.Checks.Error != "old" || r.Task.Comments[0].Body != "one" || len(r.Task.Comments) != 1 {
		t.Fatalf("the record changed with its clone: %+v", r)
	}
}

// A nil stays nil and an empty slice stays empty: the record's JSON tells them apart («[]» and «null»).
func TestRecordCloneKeepsNilAndEmpty(t *testing.T) {
	r := Record{Task: Task{Labels: []string{}, Components: nil, Comments: []Comment{}}, AutoFixed: map[string]string{}}
	c := r.Clone()
	if c.Task.Labels == nil || c.Task.Components != nil || c.Task.Comments == nil || c.AutoFixed == nil {
		t.Fatalf("nil and empty changed: %+v", c)
	}
	want, _ := json.Marshal(r)
	got, _ := json.Marshal(c)
	if string(want) != string(got) {
		t.Fatalf("the clone's JSON differs:\n%s\n%s", want, got)
	}
	if (Record{}).Clone().Checks != nil || (Record{}).Clone().Metrics != nil {
		t.Fatal("a nil pointer became a value")
	}
}
