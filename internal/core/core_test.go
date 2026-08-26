package core

import (
	"reflect"
	"testing"
)

// TestTheCoreContractsCarryNothingOfAnExtension pins the boundary an extension
// stays outside of: the core knows a message received, a message sent and a turn.
func TestTheCoreContractsCarryNothingOfAnExtension(t *testing.T) {
	for name, tc := range map[string]struct {
		methods []string
		want    []string
	}{
		"Handler": {methods: methodsOf(reflect.TypeFor[Handler]()), want: []string{"Received", "Sent"}},
		"Agent":   {methods: methodsOf(reflect.TypeFor[Agent]()), want: []string{"Turn"}},
	} {
		t.Run(name, func(t *testing.T) {
			if !reflect.DeepEqual(tc.methods, tc.want) {
				t.Fatalf("%s has %v, want %v", name, tc.methods, tc.want)
			}
		})
	}
}

func methodsOf(t reflect.Type) []string {
	names := make([]string, 0, t.NumMethod())
	for i := range t.NumMethod() {
		names = append(names, t.Method(i).Name)
	}
	return names
}
