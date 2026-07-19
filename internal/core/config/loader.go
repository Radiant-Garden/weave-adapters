package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/knadh/koanf/parsers/toml/v2"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Values holds the resolved, typed value of every registered key. It is the
// loader's output: core builds its Config from it, and each adapter builds its
// own configuration struct from the same instance, so one precedence pass
// serves both.
type Values struct {
	spec   map[string]Key
	values map[string]any
}

// String returns a TypeString key's value.
func (v *Values) String(name string) string { return get[string](v, name, TypeString) }

// Int returns a TypeInt key's value.
func (v *Values) Int(name string) int { return get[int](v, name, TypeInt) }

// Bool returns a TypeBool key's value.
func (v *Values) Bool(name string) bool { return get[bool](v, name, TypeBool) }

// Duration returns a TypeDuration key's value.
func (v *Values) Duration(name string) time.Duration {
	return get[time.Duration](v, name, TypeDuration)
}

// get reads a key of a known type. Asking for an unregistered key, or for the
// wrong type, is a wiring mistake rather than an operator one — there is no
// input that causes it and no runtime handling that would help — so it panics.
// Every getter runs during startup wiring, so a mistake surfaces on the first
// run rather than on some later request.
func get[T any](v *Values, name string, want Type) T {
	k, ok := v.spec[name]
	if !ok {
		panic(fmt.Sprintf("config: key %q is not registered", name))
	}

	if k.Type != want {
		panic(fmt.Sprintf("config: key %q is of type %s, read as %s", name, k.Type, want))
	}

	typed, ok := v.values[name].(T)
	if !ok {
		panic(fmt.Sprintf("config: key %q did not resolve to type %s", name, want))
	}

	return typed
}

// Load reads every key in spec from flags, environment (WEAVE_ADAPTER_*), an
// optional TOML file, and the registered defaults — in that order of
// precedence. args are the CLI arguments without the program name.
//
// It reports type failures but not semantic ones: turning Values into a
// validated struct is Core's job, and each adapter's own.
func Load(spec Spec, args []string) (*Values, error) {
	return load(spec, args, os.Environ)
}

// load is the testable core of Load with an injectable environment source.
func load(spec Spec, args []string, environ func() []string) (*Values, error) {
	index, err := spec.index()
	if err != nil {
		return nil, err
	}

	configPath, overrides, err := parseFlags(spec, args)
	if err != nil {
		return nil, err
	}

	k := koanf.New(".")

	if err := k.Load(confmap.Provider(defaults(spec), "."), nil); err != nil {
		return nil, fmt.Errorf("loading defaults: %w", err)
	}

	if configPath != "" {
		if err := k.Load(file.Provider(configPath), toml.Parser()); err != nil {
			return nil, fmt.Errorf("loading config file %q: %w", configPath, err)
		}
	}

	envProvider := env.Provider(".", env.Opt{
		Prefix:        envPrefix,
		EnvironFunc:   environ,
		TransformFunc: envTransform(spec),
	})
	if err := k.Load(envProvider, nil); err != nil {
		return nil, fmt.Errorf("loading environment: %w", err)
	}

	if len(overrides) > 0 {
		if err := k.Load(confmap.Provider(overrides, "."), nil); err != nil {
			return nil, fmt.Errorf("loading flags: %w", err)
		}
	}

	return resolve(spec, index, k)
}

// resolve coerces every registered key to its declared type, collecting every
// failure rather than stopping at the first — an operator fixing configuration
// should see all of it in one run, which is the same reason Validate joins.
func resolve(spec Spec, index map[string]Key, k *koanf.Koanf) (*Values, error) {
	var errs []error

	values := make(map[string]any, len(spec))

	for _, key := range spec {
		typed, err := coerce(key, k.Get(key.Name))
		if err != nil {
			errs = append(errs, err)

			continue
		}

		values[key.Name] = typed
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return &Values{spec: index, values: values}, nil
}

// defaults returns the built-in values for the keys that declare one.
func defaults(spec Spec) map[string]any {
	out := make(map[string]any, len(spec))

	for _, k := range spec {
		if k.Default != nil {
			out[k.Name] = k.Default
		}
	}

	return out
}

// parseFlags registers a flag per key and returns the config file path plus the
// keys the user explicitly set. Only visited flags win, so an unset flag's zero
// value doesn't clobber a file or environment value.
func parseFlags(spec Spec, args []string) (configPath string, overrides map[string]any, err error) {
	fs := flag.NewFlagSet("weave-adapter-dhcp-windows", flag.ContinueOnError)

	// Flag name -> key name, and flag name -> the pointer holding its value.
	keyOf := make(map[string]string, len(spec))
	valueOf := make(map[string]any, len(spec))

	for _, k := range spec {
		name := FlagName(k.Name)
		keyOf[name] = k.Name

		switch k.Type {
		case TypeString:
			valueOf[name] = fs.String(name, "", k.Usage)
		case TypeInt:
			valueOf[name] = fs.Int(name, 0, k.Usage)
		case TypeBool:
			valueOf[name] = fs.Bool(name, false, k.Usage)
		case TypeDuration:
			valueOf[name] = fs.Duration(name, 0, k.Usage)
		default:
			return "", nil, fmt.Errorf("config key %q has an unknown type %d", k.Name, k.Type)
		}
	}

	fs.StringVar(&configPath, "config", "", "path to a TOML config file")

	if err = fs.Parse(args); err != nil {
		return "", nil, err
	}

	overrides = make(map[string]any)

	fs.Visit(func(f *flag.Flag) {
		key, ok := keyOf[f.Name]
		if !ok {
			return // -config, which is not a registered key.
		}

		switch p := valueOf[f.Name].(type) {
		case *string:
			overrides[key] = *p
		case *int:
			overrides[key] = *p
		case *bool:
			overrides[key] = *p
		case *time.Duration:
			overrides[key] = *p
		}
	})

	return configPath, overrides, nil
}

// envTransform returns the koanf TransformFunc mapping WEAVE_ADAPTER_*
// variables onto registered keys. Unrecognized variables return an empty key
// and are ignored.
//
// Values pass through as strings: typing happens in one place, in resolve, so
// that a value from the environment and the same value from a TOML file are
// read by the same parser and produce the same error.
func envTransform(spec Spec) func(string, string) (string, any) {
	keyOf := make(map[string]string, len(spec))
	for _, k := range spec {
		keyOf[EnvName(k.Name)] = k.Name
	}

	return func(name, value string) (string, any) {
		key, ok := keyOf[name]
		if !ok {
			return "", nil
		}

		return key, value
	}
}
