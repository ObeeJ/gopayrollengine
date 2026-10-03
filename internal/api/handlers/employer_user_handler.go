package handlers

import (
	"errors"
	"net/http"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/services"

	"github.com/gin-gonic/gin"
)

// EmployerUserHandler — admin management of the people who log in to an org.
type EmployerUserHandler struct {
	Users *services.EmployerUserService
}

func actorOf(c *gin.Context) services.Actor {
	return services.Actor{Name: middleware.ActorName(c), IP: c.ClientIP()}
}

func (h *EmployerUserHandler) respondErr(c *gin.Context, op string, err error) {
	switch {
	case errors.Is(err, services.ErrEmployerUserNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
	case errors.Is(err, services.ErrEmployerUserExists), errors.Is(err, services.ErrLastAdmin):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, services.ErrInvalidEmployerUser):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		middleware.Logger.Error(op+" failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}

// Create handles POST /users — returns the temporary password exactly once.
func (h *EmployerUserHandler) Create(c *gin.Context) {
	var req struct {
		Email string `json:"email" binding:"required"`
		Name  string `json:"name"`
		Role  string `json:"role" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBindError(c, err)
		return
	}
	u, temp, err := h.Users.Create(c.Request.Context(), middleware.OrgID(c), req.Email, req.Name, req.Role, actorOf(c))
	if err != nil {
		h.respondErr(c, "create employer user", err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"user": u, "temporary_password": temp,
		"note": "shown once; the person must change it at first login"})
}

// List handles GET /users.
func (h *EmployerUserHandler) List(c *gin.Context) {
	users, err := h.Users.List(c.Request.Context(), middleware.OrgID(c))
	if err != nil {
		h.respondErr(c, "list employer users", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"users": users})
}

// Update handles PATCH /users/:id — role and/or is_active.
func (h *EmployerUserHandler) Update(c *gin.Context) {
	var req struct {
		Role     *string `json:"role"`
		IsActive *bool   `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBindError(c, err)
		return
	}
	u, err := h.Users.Update(c.Request.Context(), middleware.OrgID(c), c.Param("id"),
		services.EmployerUserUpdate{Role: req.Role, IsActive: req.IsActive}, actorOf(c))
	if err != nil {
		h.respondErr(c, "update employer user", err)
		return
	}
	c.JSON(http.StatusOK, u)
}

// ResetPassword handles POST /users/:id/reset-password.
func (h *EmployerUserHandler) ResetPassword(c *gin.Context) {
	temp, err := h.Users.ResetPassword(c.Request.Context(), middleware.OrgID(c), c.Param("id"), actorOf(c))
	if err != nil {
		h.respondErr(c, "reset employer password", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"temporary_password": temp,
		"note": "shown once; the person must change it at next login"})
}
