package main

import "testing"

func TestRetainedSystemdExitIsRecognized(t *testing.T) {
	raw := "ActiveState=active\nSubState=exited\nResult=success\nExecMainStatus=0\nExecMainCode=1\n"
	done, err := serviceOutcome(raw)
	if !done || err != nil {
		t.Fatalf("retained clean exit must complete: done=%v err=%v", done, err)
	}
	for _, raw := range []string{
		"ActiveState=active\nSubState=exited\nResult=success\nExecMainStatus=0\nExecMainCode=2\n",
		"ActiveState=active\nSubState=exited\nResult=success\nExecMainStatus=1\nExecMainCode=1\n",
	} {
		done, err := serviceOutcome(raw)
		if !done || err == nil {
			t.Fatalf("unclean retained exit must fail: %v %v", done, err)
		}
	}
}
