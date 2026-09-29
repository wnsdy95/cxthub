package inbound

import "context"

// CodeCheckedCheckout is the manual working-position transition. Hook callers
// use Checkout after recording their observed Git operation separately.
type CodeCheckedCheckout interface {
	CheckoutAtCode(context.Context, CheckoutInput, string) (CheckoutOutput, error)
}
