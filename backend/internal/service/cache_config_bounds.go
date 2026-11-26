package service

import (
	"fmt"
	"math"
	"reflect"
	"strings"
)

// Bound persisted numbers before duration arithmetic, jitter ranges and token multiplication.
func validateCacheNumericBounds(c CacheStrategyConfig) error {
	var walk func(reflect.Value, string) error
	walk = func(v reflect.Value, path string) error {
		switch v.Kind() {
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				key := strings.Split(v.Type().Field(i).Tag.Get("json"), ",")[0]
				if err := walk(v.Field(i), path+"."+key); err != nil {
					return err
				}
			}
		case reflect.Map:
			for _, key := range v.MapKeys() {
				if err := walk(v.MapIndex(key), path+"."+key.String()); err != nil {
					return err
				}
			}
		case reflect.Int, reflect.Int64:
			limit := int64(cacheAbsoluteMaxInputTokens)
			if strings.HasSuffix(path, "estimated_bytes_limit") {
				limit = 16 << 30
			}
			if v.Int() < 0 || v.Int() > limit {
				return fmt.Errorf("%s must be between 0 and %d", path, limit)
			}
		case reflect.Float64:
			if math.IsNaN(v.Float()) || math.IsInf(v.Float(), 0) || v.Float() > 1000 {
				return fmt.Errorf("%s must be finite and at most 1000", path)
			}
		}
		return nil
	}
	return walk(reflect.ValueOf(c), "config")
}
