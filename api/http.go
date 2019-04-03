package api

import (
	"github.com/gin-gonic/gin"
)

type ApiServer struct {
	httpListenAddr string
}

func (api *ApiServer) InitBtcClient(btcAddr string) error {
	return nil
}

func NewApiServer(addr string) (*ApiServer, error) {
	return &ApiServer{
		httpListenAddr: addr,
	}, nil
}

func (api *ApiServer) HttpListen() error {
	r := gin.Default()
	r.GET("/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "pong",
		})
	})
	return r.Run(api.httpListenAddr)
}
