package runtime

import (
	transportmcp "github.com/arksnorman/open-kase/api/internal/transport/mcp"
)

// mcpHandler builds the /mcp endpoint over the same application services
// as the REST adapter (one instance per container).
func (c *Container) mcpHandler() *transportmcp.Handler {
	if c.MCP == nil {
		services := c.httpServices()
		c.MCP = transportmcp.New(transportmcp.Deps{
			Logger: c.Log, Access: c.Access, Tickets: c.Tickets, Projects: c.Projects, Customers: c.Customers,
			Macros: c.Macros, HoldReasons: c.HoldReasons, Organisations: c.Organisations, Workflow: c.Workflow,
			Idempotency: c.Idempotency, Audit: c.Audit, Clock: c.Clock, BaseURL: services.BaseURL,
		})
	}
	return c.MCP
}
