package routers

import (
	"os"

	"github.com/gin-gonic/gin"
)

var apikey = os.Getenv("APIKEY")

func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if apikey == "" {
			c.Next()
			return
		}

		if c.Request.Header.Get("Authorization") != "Bearer "+apikey {
			ResponseError(c, CodeForbidden)
			c.Abort()
			return
		}

		c.Next()
	}
}
