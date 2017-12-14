package command_run

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/mysterium/node/bytescount_client"
	"github.com/mysterium/node/communication"
	"github.com/mysterium/node/identity"
	"github.com/mysterium/node/openvpn"
	vpn_session "github.com/mysterium/node/openvpn/session"
	"github.com/mysterium/node/server"
	dto_discovery "github.com/mysterium/node/service_discovery/dto"
	"github.com/mysterium/node/tequilapi"
)

type CommandRun struct {
	Output      io.Writer
	OutputError io.Writer

	MysteriumClient server.Client

	CommunicationClientFactory func(identity dto_discovery.Identity) communication.Client
	communicationClient        communication.Client

	vpnMiddlewares []openvpn.ManagementMiddleware
	vpnClient      *openvpn.Client

	httpApiServer tequilapi.ApiServer
}

func (cmd *CommandRun) Run(options CommandOptions) (err error) {
	consumerId := dto_discovery.Identity("consumer1")

	session, err := cmd.MysteriumClient.SessionCreate(options.NodeKey)
	if err != nil {
		return err
	}

	cmd.communicationClient = cmd.CommunicationClientFactory(consumerId)
	proposal := session.ServiceProposal
	sender, _, err := cmd.communicationClient.CreateDialog(proposal.ProviderContacts[0].Definition)
	if err != nil {
		return err
	}

	vpnSession, err := getVpnSession(sender, strconv.Itoa(proposal.Id))
	if err != nil {
		return err
	}

	vpnConfig, err := openvpn.NewClientConfigFromString(
		vpnSession.Config,
		options.DirectoryRuntime+"/client.ovpn",
	)
	if err != nil {
		return err
	}

	vpnMiddlewares := append(
		cmd.vpnMiddlewares,
		bytescount_client.NewMiddleware(cmd.MysteriumClient, session.Id, 1*time.Minute),
	)
	cmd.vpnClient = openvpn.NewClient(
		vpnConfig,
		options.DirectoryRuntime,
		vpnMiddlewares...,
	)
	if err := cmd.vpnClient.Start(); err != nil {
		return err
	}

	// options.keystoreDir still to be implemented. represents keystore directory/file
	keystoreInstance := keystore.NewKeyStore(options.DirectoryKeystore, keystore.StandardScryptN, keystore.StandardScryptP)
	idm := identity.NewIdentityManager(keystoreInstance)
	router := tequilapi.NewApiEndpoints(idm)

	cmd.httpApiServer, err = tequilapi.StartNewServer(options.TequilaApiAddress, options.TequilaApiPort, router)
	if err != nil {
		return err
	}

	return nil
}

func (cmd *CommandRun) Wait() error {
	return cmd.httpApiServer.Wait()
}

func (cmd *CommandRun) Kill() {
	cmd.httpApiServer.Stop()
	cmd.communicationClient.Stop()
	cmd.vpnClient.Stop()
}

func getVpnSession(sender communication.Sender, proposalId string) (session vpn_session.VpnSession, err error) {
	sessionJson, err := sender.Request(communication.SESSION_CREATE, proposalId)
	if err != nil {
		return
	}

	var response vpn_session.SessionCreateResponse

	err = json.Unmarshal([]byte(sessionJson), &response)
	if err != nil {
		return
	}

	if response.Success == false {
		return session, errors.New(response.Message)
	}

	return response.Session, nil
}
