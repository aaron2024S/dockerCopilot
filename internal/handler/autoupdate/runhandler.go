package autoupdate

import (
	"net/http"

	"github.com/onlyLTY/dockerCopilot/internal/logic/autoupdate"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

func RunHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := autoupdate.NewRunLogic(r.Context(), svcCtx)
		resp, err := l.Run()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
