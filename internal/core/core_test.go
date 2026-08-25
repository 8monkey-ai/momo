package core

import (
	"reflect"
	"testing"
)

// TestTheCoreContractsHoldOnlyTheirOwnMethods pins the base contracts: an
// optional extension, such as session-history recording, is a port of its own
// package and never a method the core makes every channel and agent carry.
func TestTheCoreContractsHoldOnlyTheirOwnMethods(t *testing.T) {
	for name, tc := range map[string]struct {
		contract reflect.Type
		want     []string
	}{
		"Handler": {contract: reflect.TypeOf((*Handler)(nil)).Elem(), want: []string{"Received", "Sent"}},
		"Agent":   {contract: reflect.TypeOf((*Agent)(nil)).Elem(), want: []string{"Turn"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := make([]string, 0, tc.contract.NumMethod())
			for i := range tc.contract.NumMethod() {
				got = append(got, tc.contract.Method(i).Name)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s has %v, want %v", name, got, tc.want)
			}
		})
	}
}
