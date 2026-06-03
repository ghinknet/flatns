package config

import (
	"reflect"
	"time"

	"github.com/mitchellh/mapstructure"
)

// durationHook lets users express FlattenConfig.Interval as a human-friendly
// string such as "5m" or "30s" in the YAML file while still decoding into a
// time.Duration field. mapstructure (used internally by viper) calls this when
// the source kind is string and the target type is time.Duration.
func durationHook() mapstructure.DecodeHookFunc {
	return func(from reflect.Type, to reflect.Type, data any) (any, error) {
		if to != reflect.TypeOf(time.Duration(0)) {
			return data, nil
		}
		switch from.Kind() {
		case reflect.String:
			return time.ParseDuration(data.(string))
		case reflect.Int, reflect.Int64:
			// Bare integers are interpreted as seconds for convenience.
			return time.Duration(reflect.ValueOf(data).Int()) * time.Second, nil
		case reflect.Float64:
			return time.Duration(data.(float64)) * time.Second, nil
		default:
			return data, nil
		}
	}
}
