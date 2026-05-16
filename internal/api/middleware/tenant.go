package middleware

import (
	"go-payroll-engine/internal/models"
	"net/http"

	"github.com/gin-gonic/gin"
)

const OrgIDKey = "org_id"

// TenantMiddleware — confirms org_id from JWT is valid and loads the org's data region.
// Aborts if org_id is missing or the org doesn't exist — no ghost tenants allowed.
func TenantMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		orgID := OrgID(c)
		if orgID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "tenant identity missing \u2014 is your token valid?"})
			c.Abort()
			return
		}

		// Load the org's data_region so DataResidency middleware can enforce it.
		var org models.Organization
		if err := models.DB.Select("id, data_region, is_active").First(&org, "id = ?", orgID).Error; err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "organization not found"})
			c.Abort()
			return
		}
		if !org.IsActive {
			c.JSON(http.StatusForbidden, gin.H{"error": "organization is suspended"})
			c.Abort()
			return
		}

		c.Set("org_region", org.DataRegion)
		c.Next()
	}
}

// OrgID \u2014 reads org_id from context set by JWTAuth; empty string = JWTAuth didn't run (bug).
func OrgID(c *gin.Context) string {
	v, _ := c.Get(OrgIDKey)
	id, _ := v.(string)
	return id
}
