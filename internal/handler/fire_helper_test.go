package handler

import (
	"context"

	"github.com/kfet/acp-kit/schedule"
)

// fireForTest runs FireSchedule for a test that only checks the error.
func (h *Handler) fireForTest(ctx context.Context, it schedule.Item) error {
	_, err := h.FireSchedule(ctx, it)
	return err
}
