// Copyright 2020 Lingfei Kong <colin404@foxmail.com>. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package authzserver

import (
	"github.com/gin-gonic/gin"
	"github.com/marmotedu/component-base/pkg/core"
	"github.com/marmotedu/errors"

	"github.com/marmotedu/iam/internal/authzserver/api/v1/authorize"
	"github.com/marmotedu/iam/internal/authzserver/store"
	"github.com/marmotedu/iam/internal/pkg/code"
	"github.com/marmotedu/iam/internal/pkg/middleware"
)

func installHandler(g *gin.Engine) *gin.Engine {
	authMiddleware, _ := middleware.NewAuthMiddleware(nil, newAuthzServerJwt())
	g.NoRoute(authMiddleware.AuthCacheMiddlewareFunc(), func(c *gin.Context) {
		core.WriteResponse(c, errors.WithCode(code.ErrPageNotFound, "page not found."), nil)
	})

	storeIns, _ := store.GetStoreInsOr(nil)
	apiv1 := g.Group("/v1", authMiddleware.AuthCacheMiddlewareFunc())
	{
		authzHandler := authorize.NewAuthzHandler(storeIns)

		// Router for authorization
		apiv1.POST("/authz", authzHandler.Authorize)
	}

	return g
}
