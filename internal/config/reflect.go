package config

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// applyDefaults populates every field from its `default:` struct tag.
//
// Defaults live on the struct rather than in a separate table so they cannot
// drift from the fields they describe — the same reason `config reference` is
// generated rather than hand-written (§11.2).
func applyDefaults(v any) {
	walk(reflect.ValueOf(v).Elem(), nil, func(f reflect.Value, sf reflect.StructField, _ []string) error {
		def, ok := sf.Tag.Lookup("default")
		if !ok {
			return nil
		}
		return setFromString(f, def)
	})
}

// applyEnv overlays MESHBBS_<SECTION>_<KEY> environment variables (§11.2).
func applyEnv(v any) error {
	return walk(reflect.ValueOf(v).Elem(), nil, func(f reflect.Value, sf reflect.StructField, path []string) error {
		name := envName(path)
		raw, ok := os.LookupEnv(name)
		if !ok {
			return nil
		}
		if err := setFromString(f, raw); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	})
}

func envName(path []string) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = strings.ToUpper(p)
	}
	return "MESHBBS_" + strings.Join(parts, "_")
}

// walk visits every leaf field, passing its dotted TOML path.
//
// Note this iterates struct fields by index, never a map — field order is
// fixed by the type, so traversal is deterministic (§6.2.1 rule 2 in spirit:
// generated output must not vary run to run).
func walk(v reflect.Value, path []string, fn func(reflect.Value, reflect.StructField, []string) error) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name := sf.Tag.Get("toml")
		if name == "" {
			name = strings.ToLower(sf.Name)
		}
		fieldPath := append(append([]string(nil), path...), name)
		f := v.Field(i)

		if f.Kind() == reflect.Struct {
			if err := walk(f, fieldPath, fn); err != nil {
				return err
			}
			continue
		}
		if err := fn(f, sf, fieldPath); err != nil {
			return err
		}
	}
	return nil
}

func setFromString(f reflect.Value, s string) error {
	switch f.Kind() {
	case reflect.String:
		f.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("%q is not a boolean", s)
		}
		f.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not an integer", s)
		}
		f.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not a non-negative integer", s)
		}
		f.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("%q is not a number", s)
		}
		f.SetFloat(n)
	default:
		return fmt.Errorf("unsupported config field kind %s", f.Kind())
	}
	return nil
}

// Entry describes one configuration key for the generated reference.
type Entry struct {
	Key     string
	Type    string
	Default string
	Doc     string
	Env     string
}

// Reference returns every configuration key, generated from the struct
// definitions so it cannot drift from the code (§11.2).
func Reference() []Entry {
	var out []Entry
	var c Config
	sectionDocs := map[string]string{}

	// Section docs come from the top-level fields.
	ct := reflect.TypeOf(c)
	for i := 0; i < ct.NumField(); i++ {
		sf := ct.Field(i)
		if name := sf.Tag.Get("toml"); name != "" {
			sectionDocs[name] = sf.Tag.Get("doc")
		}
	}

	_ = walk(reflect.ValueOf(&c).Elem(), nil, func(f reflect.Value, sf reflect.StructField, path []string) error {
		out = append(out, Entry{
			Key:     strings.Join(path, "."),
			Type:    f.Kind().String(),
			Default: sf.Tag.Get("default"),
			Doc:     sf.Tag.Get("doc"),
			Env:     envName(path),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// SectionDoc returns the documentation for a top-level section.
func SectionDoc(section string) string {
	ct := reflect.TypeOf(Config{})
	for i := 0; i < ct.NumField(); i++ {
		sf := ct.Field(i)
		if sf.Tag.Get("toml") == section {
			return sf.Tag.Get("doc")
		}
	}
	return ""
}
