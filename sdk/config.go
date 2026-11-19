package sdk

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	chains "chains-automation"
	"github.com/pelletier/go-toml/v2"
)

// Config contains options shared by Go and Python services.
type Config struct {
	URI               string          `toml:"uri" flag:"bus" env:"BUS"`
	Namespace         string          `toml:"namespace"`
	Encoding          chains.Encoding `toml:"encoding"`
	Authority         string          `toml:"authority"`
	HeartbeatInterval int             `toml:"heartbeat_interval"`
	PollInterval      float64         `toml:"poll_interval"`
	ServiceID         string          `toml:"service_id"`
	Name              string          `toml:"name" flag:"service-name" env:"SERVICE_NAME"`
	Instance          string          `toml:"-"`
}

type configField struct {
	path            []string
	flag, env, help string
	field           reflect.Value
	required        bool
}

var settingName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)

func xdgPath(variable, fallback string) string {
	if value := os.Getenv(variable); filepath.IsAbs(value) {
		return value
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

func serviceName(s string) bool { return chains.Segment(s) && s != "." && s != ".." }

// LoadConfig applies service defaults, XDG or explicit TOML, arguments, then environment.
// settings is an initialized pointer to the service's settings struct.
func LoadConfig(serviceType string, args []string, settings any) (Config, error) {
	c := Config{URI: "mqtt://localhost:1883", Namespace: "chains", Encoding: chains.JSON, Authority: "engine", HeartbeatInterval: 15, PollInterval: 1}
	if !serviceName(serviceType) {
		return c, errors.New("invalid service type")
	}
	f := flag.NewFlagSet(serviceType, flag.ContinueOnError)
	var explicit, instance string
	f.StringVar(&explicit, "config", "", "Path to a service TOML file")
	f.StringVar(&explicit, "f", "", "Path to a service TOML file")
	f.StringVar(&instance, "instance", "", "Service instance name")
	common, err := configFields(reflect.ValueOf(&c).Elem(), nil, true)
	if err != nil {
		return c, err
	}
	var custom []*configField
	if settings != nil {
		value := reflect.ValueOf(settings)
		if value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Kind() != reflect.Struct {
			return c, errors.New("settings must be a pointer to a struct")
		}
		custom, err = configFields(value.Elem(), nil, false)
		if err != nil {
			return c, err
		}
	}
	all := append(common, custom...)
	cli := map[string]*string{}
	for _, entry := range all {
		if f.Lookup(entry.flag) != nil || entry.flag == "help" || entry.flag == "h" {
			return c, fmt.Errorf("conflicting config flag --%s", entry.flag)
		}
		cli[entry.flag] = f.String(entry.flag, "", entry.help)
	}
	if err = f.Parse(args); err != nil {
		return c, err
	}
	if f.NArg() != 0 {
		return c, errors.New("unexpected positional arguments")
	}
	supplied := map[string]bool{}
	f.Visit(func(current *flag.Flag) { supplied[current.Name] = true })
	if path, ok := os.LookupEnv("CHAINS_SERVICE_CONFIG"); ok {
		explicit = path
	}
	if value, ok := os.LookupEnv("CHAINS_INSTANCE"); ok {
		instance = value
	} else if !supplied["instance"] {
		instance = serviceType
		if explicit != "" {
			name := filepath.Base(explicit)
			instance = strings.TrimSuffix(name, filepath.Ext(name))
		}
	}
	if !serviceName(instance) {
		return c, errors.New("invalid service instance")
	}
	c.Instance, c.Name = instance, instance
	var paths []string
	if explicit != "" {
		paths = []string{explicit}
	} else {
		dirs := os.Getenv("XDG_CONFIG_DIRS")
		if dirs == "" {
			dirs = "/etc/xdg"
		}
		roots := strings.Split(dirs, ":")
		for i := len(roots) - 1; i >= 0; i-- {
			if filepath.IsAbs(roots[i]) {
				paths = append(paths, filepath.Join(roots[i], "chains", "services", instance+".toml"))
			}
		}
		paths = append(paths, filepath.Join(xdgPath("XDG_CONFIG_HOME", ".config"), "chains", "services", instance+".toml"))
	}
	effective := map[*configField]any{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) && explicit == "" {
			continue
		}
		if err != nil {
			return c, err
		}
		var document map[string]any
		if toml.Unmarshal(raw, &document) != nil {
			return c, fmt.Errorf("invalid TOML in %s", path)
		}
		for name, value := range document {
			var section []*configField
			switch name {
			case "service":
				section = common
			case "settings":
				section = custom
			default:
				return c, fmt.Errorf("unknown config section %s", name)
			}
			if err := collectConfig(effective, section, name, nil, value); err != nil {
				return c, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	variables := map[string]bool{}
	for _, entry := range all {
		variables[entry.env] = true
	}
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(name, "CHAINS_SETTINGS__") && !variables[name] {
			return c, fmt.Errorf("unknown service settings variable %s", name)
		}
	}
	for _, entry := range all {
		value, ok := effective[entry]
		raw, override := os.LookupEnv(entry.env)
		if !override && supplied[entry.flag] {
			raw, override = *cli[entry.flag], true
		}
		if override {
			value, err = entry.parse(raw)
			if err != nil {
				return c, err
			}
			ok = true
		}
		if ok {
			converted, valid := convertSetting(value, entry.field.Type())
			if !valid {
				return c, fmt.Errorf("invalid value for %s", strings.Join(entry.path, "."))
			}
			entry.field.Set(converted)
		} else if entry.required && entry.field.IsZero() {
			return c, fmt.Errorf("missing required setting %s", strings.Join(entry.path, "."))
		}
	}
	if c.Name == "" {
		c.Name = instance
	}
	return c, c.validate()
}

func configFields(value reflect.Value, parents []string, common bool) ([]*configField, error) {
	var result []*configField
	for i := 0; i < value.NumField(); i++ {
		field := value.Type().Field(i)
		name := field.Tag.Get("toml")
		if field.PkgPath != "" || name == "-" {
			continue
		}
		if !settingName.MatchString(name) {
			return nil, fmt.Errorf("field %s requires a snake-case toml tag", field.Name)
		}
		path := append(append([]string{}, parents...), name)
		target := value.Field(i)
		if target.Kind() == reflect.Struct && !common {
			nested, err := configFields(target, path, false)
			if err != nil {
				return nil, err
			}
			result = append(result, nested...)
			continue
		}
		flagName := strings.ReplaceAll(strings.Join(path, "-"), "_", "-")
		variable := "SETTINGS__" + strings.ToUpper(strings.Join(path, "__"))
		root := "settings"
		if common {
			root = "service"
			variable = strings.ToUpper(name)
			if alias := field.Tag.Get("flag"); alias != "" {
				flagName = alias
			}
			if alias := field.Tag.Get("env"); alias != "" {
				variable = alias
			}
		}
		path = append([]string{root}, path...)
		help := field.Tag.Get("help")
		if help == "" {
			help = strings.Join(path, ".")
		}
		if !settingType(target.Type()) {
			return nil, fmt.Errorf("unsupported setting type for %s", help)
		}
		result = append(result, &configField{path: path, flag: flagName, env: "CHAINS_" + variable, help: help, field: target, required: field.Tag.Get("required") == "true"})
	}
	return result, nil
}

func settingType(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	case reflect.Pointer, reflect.Slice:
		return settingType(t.Elem())
	case reflect.Map:
		return t.Key().Kind() == reflect.String && settingType(t.Elem())
	}
	return false
}

func collectConfig(dst map[*configField]any, fields []*configField, root string, parent []string, value any) error {
	table, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("expected table for %s", strings.Join(append([]string{root}, parent...), "."))
	}
	for name, child := range table {
		path := append(append([]string{root}, parent...), name)
		var matches []*configField
		for _, entry := range fields {
			if len(entry.path) < len(path) || strings.Join(entry.path[:len(path)], ".") != strings.Join(path, ".") {
				continue
			}
			matches = append(matches, entry)
		}
		if len(matches) == 0 {
			return fmt.Errorf("unknown setting %s", strings.Join(path, "."))
		}
		if len(matches[0].path) == len(path) {
			dst[matches[0]] = child
			continue
		}
		if err := collectConfig(dst, matches, root, path[1:], child); err != nil {
			return err
		}
	}
	return nil
}

func (e *configField) parse(raw string) (any, error) {
	t := e.field.Type()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.String {
		return raw, nil
	}
	var doc map[string]any
	if toml.Unmarshal([]byte("value = "+raw), &doc) != nil || len(doc) != 1 || doc["value"] == nil {
		return nil, fmt.Errorf("invalid TOML value for %s", strings.Join(e.path, "."))
	}
	return doc["value"], nil
}

func convertSetting(value any, t reflect.Type) (reflect.Value, bool) {
	out := reflect.New(t).Elem()
	switch t.Kind() {
	case reflect.String:
		v, ok := value.(string)
		if ok {
			out.SetString(v)
		}
		return out, ok
	case reflect.Bool:
		v, ok := value.(bool)
		if ok {
			out.SetBool(v)
		}
		return out, ok
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v, ok := value.(int64)
		if !ok || out.OverflowInt(v) {
			return out, false
		}
		out.SetInt(v)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v, ok := value.(int64)
		if !ok || v < 0 || out.OverflowUint(uint64(v)) {
			return out, false
		}
		out.SetUint(uint64(v))
	case reflect.Float32, reflect.Float64:
		var n float64
		switch v := value.(type) {
		case float64:
			n = v
		case int64:
			n = float64(v)
		default:
			return out, false
		}
		if out.OverflowFloat(n) {
			return out, false
		}
		out.SetFloat(n)
	case reflect.Pointer:
		v, ok := convertSetting(value, t.Elem())
		if !ok {
			return out, false
		}
		out.Set(reflect.New(t.Elem()))
		out.Elem().Set(v)
	case reflect.Slice:
		list, ok := value.([]any)
		if !ok {
			return out, false
		}
		out.Set(reflect.MakeSlice(t, len(list), len(list)))
		for i, item := range list {
			v, ok := convertSetting(item, t.Elem())
			if !ok {
				return out, false
			}
			out.Index(i).Set(v)
		}
	case reflect.Map:
		table, ok := value.(map[string]any)
		if !ok {
			return out, false
		}
		out.Set(reflect.MakeMapWithSize(t, len(table)))
		for key, item := range table {
			v, ok := convertSetting(item, t.Elem())
			if !ok {
				return out, false
			}
			k := reflect.New(t.Key()).Elem()
			k.SetString(key)
			out.SetMapIndex(k, v)
		}
	default:
		return out, false
	}
	return out, true
}

func (c Config) validate() error {
	if c.URI == "" || !serviceName(c.Instance) || !chains.Segment(c.Namespace) || !chains.Segment(c.Authority) ||
		(c.ServiceID != "" && (!chains.Segment(c.ServiceID) || c.ServiceID == c.Authority)) {
		return errors.New("invalid service URI, instance or identity")
	}
	if c.Encoding != chains.JSON && c.Encoding != chains.CBOR {
		return errors.New("encoding must be json or cbor")
	}
	if c.HeartbeatInterval < 1 || c.HeartbeatInterval > 3600 {
		return errors.New("heartbeat interval must be between 1 and 3600 seconds")
	}
	if math.IsNaN(c.PollInterval) || c.PollInterval < 0 || c.PollInterval >= float64(math.MaxInt64)/float64(time.Second) {
		return errors.New("invalid poll interval")
	}
	return nil
}

func persistedID(serviceType, instance string) (string, error) {
	dir := filepath.Join(xdgPath("XDG_STATE_HOME", ".local/state"), "chains", "services")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, serviceType+"."+instance+".id")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		temp, err := os.CreateTemp(dir, ".identity-*")
		if err != nil {
			return "", err
		}
		defer os.Remove(temp.Name())
		_, writeErr := temp.WriteString(chains.ID() + "\n")
		if err := errors.Join(writeErr, temp.Close()); err != nil {
			return "", err
		}
		if err := os.Link(temp.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		raw, err = os.ReadFile(path)
		if err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(raw))
	if !chains.Segment(id) {
		return "", errors.New("invalid persisted service identity")
	}
	return id, nil
}
