package events

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRecorderOrderAndBound(t *testing.T) {
	r := NewRecorder(3)
	for i := 1; i <= 5; i++ {
		r.Record("k", fmt.Sprintf("msg-%d", i), nil)
	}
	got := r.List(0)
	assert.Len(t, got, 3)
	assert.Equal(t, "msg-5", got[0].Msg) // newest first
	assert.Equal(t, "msg-4", got[1].Msg)
	assert.Equal(t, "msg-3", got[2].Msg)

	limited := r.List(2)
	assert.Len(t, limited, 2)
	assert.Equal(t, "msg-5", limited[0].Msg)
}

func TestRecorderFields(t *testing.T) {
	r := NewRecorder(10)
	r.Record("image.built", "built x", map[string]string{"hook": "h"})
	got := r.List(1)
	assert.Equal(t, "image.built", got[0].Kind)
	assert.Equal(t, "h", got[0].Fields["hook"])
	assert.False(t, got[0].Time.IsZero())
}

func TestNilRecorderIsSafe(t *testing.T) {
	var r *Recorder
	r.Record("k", "m", nil) // must not panic
	assert.Nil(t, r.List(5))
}
