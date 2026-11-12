package main

import (
	"testing"

	"github.com/kfet/acp-kit/interject"
)

func TestInterjectConfigAddsLog(t *testing.T) {
	c := interjectConfig(interject.Config{Enabled: true, MinConf: 5})
	if !c.Enabled || c.MinConf != 5 || c.Logf == nil {
		t.Fatalf("config = %+v", c)
	}
}
