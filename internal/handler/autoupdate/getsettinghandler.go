package autoupdate

import (
	"net/http"

	"github.com/onlyLTY/dockerCopilot/internal/logic/autoupdate"
	"github.com/onlyLTY/dockerCopilot/internal/svc"
	"github.com/zeromicro/go-zero/rest/httpx"
)

func GetSettingHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l := autoupdate.NewGetSettingLogic(r.Context(), svcCtx)
		resp, err := l.GetSetting()
		if err != nil {
			httpx.ErrorCtx(r.Context(), w, err)
		} else {
			httpx.OkJsonCtx(r.Context(), w, resp)
		}
	}
}
