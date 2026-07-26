// Package inbound declares the cxt entry (inbound) port interfaces and DTOs.
//
// The inbound port represents "the contract (use-case interface) through which external (delivery adapter) enters the app".
// The app package implements this interface,
// and the adapters/delivery package calls this interface.
//
// Dependency rule (SPINE §3.2):
//   - This package imports only github.com/wnsdy95/cxthub/cli/internal/domain.
//   - It does not import the outbound package.
//   - It does not import any adapters/* packages.
package inbound
