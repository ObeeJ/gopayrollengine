package middleware

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// allowedRegions — the set of data regions this instance is authorised to serve.
// Set DATA_REGIONS=ng,eu to allow both; leave blank to allow all (dev only).
// In production each deployment serves exactly one region — data never crosses borders.
var allowedRegions = func() map[string]bool {
	regions := make(map[string]bool)
	raw := os.Getenv("DATA_REGIONS")
	if raw == "" {
		return regions // empty = no restriction (dev mode)
	}
	for _, r := range strings.Split(raw, ",") {
		regions[strings.TrimSpace(strings.ToLower(r))] = true
	}
	return regions
}()

// DataResidency — rejects requests from orgs whose data_region doesn't match this deployment.
// Prevents EU employee data from being processed on a Nigerian server and vice versa.
// Must run after TenantMiddleware so the org's region is available in context.
func DataResidency() gin.HandlerFunc {
	return func(c *gin.Context) {
		if len(allowedRegions) == 0 {
			c.Next() // no restriction configured — dev/single-region deployments
			return
		}

		orgRegion, _ := c.Get("org_region")
		region, _ := orgRegion.(string)
		if region == "" {
			region = "ng" // default region for existing orgs without explicit region
		}

		if !allowedRegions[strings.ToLower(region)] {
			c.JSON(http.StatusForbidden, gin.H{
				"error":  "data residency violation — this org's data cannot be processed in this region",
				"region": region,
			})
			c.Abort()
			return
		}
		c.Next()
	}
}
