// Package outbound declares the driven(outbound) port interface of cxt.
//
// The outbound port represents "the capabilities an app requires of the external world" as an interface.
// The adapters/* package implements this interface, and the app package calls this interface to execute domain logic.
//
// Dependency rule (SPINE §3.2):
//   - This package imports only github.com/wnsdy95/cxthub/cli/internal/domain.
//   - It does not import the inbound package.
//   - It does not import the adapters/* package.
package outbound
