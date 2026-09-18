package flashsale

import "context"

func (c *Consumer) CompensateOrder(ctx context.Context, order Order, cause error) error {
	return c.compensateOrder(ctx, order, cause)
}
