package handlers

import (
	"net/http"
	"time"

	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/relay"
	"github.com/bestruirui/octopus/internal/server/middleware"
	"github.com/bestruirui/octopus/internal/server/resp"
	"github.com/bestruirui/octopus/internal/server/router"
	"github.com/gin-contrib/sse"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/session").
		Use(middleware.Auth()).
		AddRoute(
			router.NewRoute("/stream", http.MethodGet).
				Handle(streamSessions),
		).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(getSessionList),
		).
		AddRoute(
			router.NewRoute("/bind", http.MethodPost).
				Handle(bindSession),
		).
		AddRoute(
			router.NewRoute("/unbind", http.MethodPost).
				Handle(unbindSession),
		)
}

// streamSessions 逐条发送建立连接时的会话快照及后续会话更新。
func streamSessions(c *gin.Context) {
	prepareSSE(c)
	snapshot, updates := relay.OpenSessionStream()
	defer relay.CloseSessionStream(updates)
	for _, session := range snapshot {
		if err := sse.Encode(c.Writer, sse.Event{Event: "session", Data: session}); err != nil {
			return
		}
		c.Writer.Flush()
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := c.Writer.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			c.Writer.Flush()
		case session, ok := <-updates:
			if !ok {
				return
			}
			if err := sse.Encode(c.Writer, sse.Event{Event: "session", Data: session}); err != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}

// getSessionList 返回全部会话的当前状态，供不使用实时连接的场景拉取。
func getSessionList(c *gin.Context) {
	resp.Success(c, relay.SessionList())
}

// bindSession 为一个会话选定上游渠道和模型；已在等待的请求随即继续，正在进行的调用被中止以便改道。
func bindSession(c *gin.Context) {
	var req model.SessionBindRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := relay.SessionBind(req.SessionID, req.ChannelID, req.ModelName); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	c.Status(http.StatusNoContent)
}

// unbindSession 解除一个会话的绑定，该会话的下一条请求重新等待人工选择。
func unbindSession(c *gin.Context) {
	var req model.SessionUnbindRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := relay.SessionUnbind(req.SessionID); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	c.Status(http.StatusNoContent)
}
