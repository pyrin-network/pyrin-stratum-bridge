package pyrinstratum

import (
	"context"
	"fmt"
	"time"

	"github.com/pyrin-network/pyrin-stratum-bridge/src/gostratum"
	"github.com/pyrin-network/pyipad/app/appmessage"
	"github.com/pyrin-network/pyipad/infrastructure/network/rpcclient"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type PyrinApi struct {
	address       string
	blockWaitTime time.Duration
	logger        *zap.SugaredLogger
	pyrind      *rpcclient.RPCClient
	connected     bool
}

func NewPyrinApi(address string, blockWaitTime time.Duration, logger *zap.SugaredLogger) (*PyrinApi, error) {
	client, err := rpcclient.NewRPCClient(address)
	if err != nil {
		return nil, err
	}

	return &PyrinApi{
		address:       address,
		blockWaitTime: blockWaitTime,
		logger:        logger.With(zap.String("component", "pyrinapi:"+address)),
		pyrind:      client,
		connected:     true,
	}, nil
}

func (ks *PyrinApi) Start(ctx context.Context, blockCb func()) {
	ks.waitForSync(true)
	go ks.startBlockTemplateListener(ctx, blockCb)
	go ks.startStatsThread(ctx)
}

func (ks *PyrinApi) startStatsThread(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			ks.logger.Warn("context cancelled, stopping stats thread")
			return
		case <-ticker.C:
			dagResponse, err := ks.pyrind.GetBlockDAGInfo()
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from pyrin, prom stats will be out of date", zap.Error(err))
				continue
			}
			response, err := ks.pyrind.EstimateNetworkHashesPerSecond(dagResponse.TipHashes[0], 1000)
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from pyrin, prom stats will be out of date", zap.Error(err))
				continue
			}
			RecordNetworkStats(response.NetworkHashesPerSecond, dagResponse.BlockCount, dagResponse.Difficulty)
		}
	}
}

func (ks *PyrinApi) reconnect() error {
	if ks.pyrind != nil {
		return ks.pyrind.Reconnect()
	}

	client, err := rpcclient.NewRPCClient(ks.address)
	if err != nil {
		return err
	}
	ks.pyrind = client
	return nil
}

func (s *PyrinApi) waitForSync(verbose bool) error {
	if verbose {
		s.logger.Info("checking pyrind sync state")
	}
	for {
		clientInfo, err := s.pyrind.GetInfo()
		if err != nil {
			return errors.Wrapf(err, "error fetching server info from pyrind @ %s", s.address)
		}
		if clientInfo.IsSynced {
			break
		}
		s.logger.Warn("Pyrin is not synced, waiting for sync before starting bridge")
		time.Sleep(5 * time.Second)
	}
	if verbose {
		s.logger.Info("pyrind synced, starting server")
	}
	return nil
}

func (s *PyrinApi) startBlockTemplateListener(ctx context.Context, blockReadyCb func()) {
	blockReadyChan := make(chan bool)
	err := s.pyrind.RegisterForNewBlockTemplateNotifications(func(_ *appmessage.NewBlockTemplateNotificationMessage) {
		blockReadyChan <- true
	})
	if err != nil {
		s.logger.Error("fatal: failed to register for block notifications from pyrin")
	}

	ticker := time.NewTicker(s.blockWaitTime)
	for {
		if err := s.waitForSync(false); err != nil {
			s.logger.Error("error checking pyrind sync state, attempting reconnect: ", err)
			if err := s.reconnect(); err != nil {
				s.logger.Error("error reconnecting to pyrind, waiting before retry: ", err)
				time.Sleep(5 * time.Second)
			}
		}
		select {
		case <-ctx.Done():
			s.logger.Warn("context cancelled, stopping block update listener")
			return
		case <-blockReadyChan:
			blockReadyCb()
			ticker.Reset(s.blockWaitTime)
		case <-ticker.C: // timeout, manually check for new blocks
			blockReadyCb()
		}
	}
}

func (ks *PyrinApi) GetBlockTemplate(
	client *gostratum.StratumContext) (*appmessage.GetBlockTemplateResponseMessage, error) {
	template, err := ks.pyrind.GetBlockTemplate(client.WalletAddr,
		fmt.Sprintf(`'%s' via pyrin-network/pyrin-stratum-bridge_%s`, client.RemoteApp, version))
	if err != nil {
		return nil, errors.Wrap(err, "failed fetching new block template from pyrin")
	}
	return template, nil
}
