package trayapp

import (
	"reflect"
	"testing"
)

func TestCTRGoArgs(t *testing.T) {
	tests := []struct {
		action Action
		want   []string
	}{
		{ActionStatus, []string{"service", "status"}},
		{ActionStart, []string{"service", "start"}},
		{ActionStop, []string{"service", "stop"}},
		{ActionRestart, []string{"service", "restart"}},
		{ActionEnableLogin, []string{"service", "enable-login"}},
		{ActionDisableLogin, []string{"service", "disable-login"}},
		{ActionDoctor, []string{"doctor"}},
	}
	for _, tt := range tests {
		got, err := CTRGoArgs(tt.action)
		if err != nil {
			t.Fatalf("CTRGoArgs(%s) failed: %v", tt.action, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("CTRGoArgs(%s) = %#v, want %#v", tt.action, got, tt.want)
		}
	}
}

func TestServiceSetupArgs(t *testing.T) {
	want := []string{"service", "install", "--start", "--start-at-login"}
	if got := ServiceSetupArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ServiceSetupArgs = %#v, want %#v", got, want)
	}
}
