package base

import (
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

type RouteRegister struct {
	DB     *gorm.DB
	Router fiber.Router
}

type PaginationControls struct {
	Limit      uint `json:"limit"`
	Page       uint `json:"page"`
	Total      uint `json:"total"`
	TotalPages uint `json:"totalPages"`
	HasMore    bool `json:"hasMore"`
}

type APIResponse struct {
	Data       any                 `json:"data"`
	Message    string              `json:"msg"`
	Pagination *PaginationControls `json:"pagination,omitempty"`
	// Meta carries list-level context (e.g. which snapshot a chart page came from).
	Meta any `json:"meta,omitempty"`
}

type APIResponseT[T any] struct {
	Data       T                   `json:"data"`
	Message    string              `json:"msg"`
	Pagination *PaginationControls `json:"pagination,omitempty"`
	Meta       map[string]any      `json:"meta,omitempty"`
}
