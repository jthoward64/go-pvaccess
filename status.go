package pvaccess

import (
	"context"
	"os"
	"runtime"
	"strings"

	"github.com/quentinmit/go-pvaccess/internal/ctxlog"
	"github.com/quentinmit/go-pvaccess/pvdata"
)

type serverChannel struct {
	srv *Server
}

func (serverChannel) Name() string {
	return "server"
}

func (c *serverChannel) CreateChannel(ctx context.Context, name string) (Channel, error) {
	if name == c.Name() {
		return c, nil
	}
	return nil, nil
}

func (c *serverChannel) ChannelRPC(ctx context.Context, args pvdata.PVStructure) (interface{}, error) {
	if strings.HasPrefix(args.ID, "epics:nt/NTURI:1.") {
		if q, ok := args.SubField("query").(*pvdata.PVStructure); ok {
			args = *q
		} else {
			return struct{}{}, pvdata.PVStatus{
				Type:    pvdata.PVStatus_ERROR,
				Message: pvdata.PVString("invalid argument"),
			}
		}
	}

	if args.SubField("help") != nil {
		// TODO
	}

	var op pvdata.PVString
	if v, ok := args.SubField("op").(*pvdata.PVString); ok {
		op = *v
	}

	ctxlog.L(ctx).Debugf("op = %s", op)

	switch op {
	case "channels":
	case "info":
		hostname, _ := os.Hostname()
		info := &struct {
			Process   string `pvaccess:"process"`
			StartTime string `pvaccess:"startTime"`
			Version   string `pvaccess:"version"`
			ImplLang  string `pvaccess:"implLang"`
			Host      string `pvaccess:"host"`
			OS        string `pvaccess:"os"`
			Arch      string `pvaccess:"arch"`
		}{
			os.Args[0],
			"sometime",
			"1.0",
			"Go",
			hostname,
			runtime.GOOS,
			runtime.GOARCH,
		}
		ctxlog.L(ctx).Debugf("returning info %+v", info)
		return info, nil
	}

	return &struct{}{}, pvdata.PVStatus{
		Type:    pvdata.PVStatus_ERROR,
		Message: pvdata.PVString("invalid argument"),
	}
}
