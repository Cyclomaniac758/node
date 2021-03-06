/*
 * Copyright (C) 2018 The "MysteriumNetwork/node" Authors.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package cmd

import (
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/mysteriumnetwork/node/communication"
	nats_dialog "github.com/mysteriumnetwork/node/communication/nats/dialog"
	"github.com/mysteriumnetwork/node/config"
	"github.com/mysteriumnetwork/node/core/connection"
	"github.com/mysteriumnetwork/node/core/node"
	"github.com/mysteriumnetwork/node/core/policy"
	"github.com/mysteriumnetwork/node/core/port"
	"github.com/mysteriumnetwork/node/core/service"
	"github.com/mysteriumnetwork/node/core/service/servicestate"
	"github.com/mysteriumnetwork/node/identity"
	"github.com/mysteriumnetwork/node/identity/registry"
	"github.com/mysteriumnetwork/node/market"
	"github.com/mysteriumnetwork/node/mmn"
	"github.com/mysteriumnetwork/node/nat"
	"github.com/mysteriumnetwork/node/nat/traversal"
	"github.com/mysteriumnetwork/node/p2p"
	service_noop "github.com/mysteriumnetwork/node/services/noop"
	service_openvpn "github.com/mysteriumnetwork/node/services/openvpn"
	openvpn_discovery "github.com/mysteriumnetwork/node/services/openvpn/discovery"
	openvpn_service "github.com/mysteriumnetwork/node/services/openvpn/service"
	"github.com/mysteriumnetwork/node/services/wireguard"
	wireguard_connection "github.com/mysteriumnetwork/node/services/wireguard/connection"
	"github.com/mysteriumnetwork/node/services/wireguard/endpoint"
	"github.com/mysteriumnetwork/node/services/wireguard/resources"
	wireguard_service "github.com/mysteriumnetwork/node/services/wireguard/service"
	"github.com/mysteriumnetwork/node/session"
	"github.com/mysteriumnetwork/node/session/connectivity"
	"github.com/mysteriumnetwork/node/session/pingpong"
	pingpong_noop "github.com/mysteriumnetwork/node/session/pingpong/noop"
	"github.com/mysteriumnetwork/node/ui"
	uinoop "github.com/mysteriumnetwork/node/ui/noop"
	"github.com/rs/zerolog/log"

	"github.com/pkg/errors"
)

// bootstrapServices loads all the components required for running services
func (di *Dependencies) bootstrapServices(nodeOptions node.Options, servicesOptions config.ServicesOptions) error {
	if nodeOptions.MobileConsumer {
		return nil
	}

	err := di.bootstrapServiceComponents(nodeOptions, servicesOptions)
	if err != nil {
		return errors.Wrap(err, "service bootstrap failed")
	}

	di.bootstrapServiceOpenvpn(nodeOptions)
	di.bootstrapServiceNoop(nodeOptions)
	di.bootstrapServiceWireguard(nodeOptions)

	return nil
}

func (di *Dependencies) bootstrapServiceWireguard(nodeOptions node.Options) {
	di.ServiceRegistry.Register(
		wireguard.ServiceType,
		func(serviceOptions service.Options) (service.Service, market.ServiceProposal, error) {
			loc, err := di.LocationResolver.DetectLocation()
			if err != nil {
				return nil, market.ServiceProposal{}, err
			}

			wgOptions := serviceOptions.(wireguard_service.Options)

			// TODO: Use global port pool once migrated to p2p.
			var portPool port.ServicePortSupplier
			var natPinger traversal.NATPinger
			if wgOptions.Ports.IsSpecified() {
				log.Info().Msgf("Fixed service port range (%s) configured, using custom port pool", wgOptions.Ports)
				portPool = port.NewFixedRangePool(*wgOptions.Ports)
				natPinger = traversal.NewNoopPinger()
			} else {
				portPool = port.NewPool()
				natPinger = di.NATPinger
			}

			svc := wireguard_service.NewManager(
				di.IPResolver,
				loc.Country,
				di.NATService,
				natPinger,
				di.NATTracker,
				di.EventBus,
				wgOptions,
				portPool,
				di.PortMapper,
				di.ServiceFirewall,
			)
			return svc, wireguard_service.GetProposal(loc), nil
		},
	)
}

func (di *Dependencies) bootstrapServiceOpenvpn(nodeOptions node.Options) {
	createService := func(serviceOptions service.Options) (service.Service, market.ServiceProposal, error) {
		if err := nodeOptions.Openvpn.Check(); err != nil {
			return nil, market.ServiceProposal{}, err
		}

		loc, err := di.LocationResolver.DetectLocation()
		if err != nil {
			return nil, market.ServiceProposal{}, err
		}

		transportOptions := serviceOptions.(openvpn_service.Options)
		proposal := openvpn_discovery.NewServiceProposalWithLocation(loc, transportOptions.Protocol)

		// TODO: Use global port pool once migrated to p2p.
		var portPool port.ServicePortSupplier
		var natPinger traversal.NATPinger
		if transportOptions.Port != 0 {
			portPool = port.NewPoolFixed(port.Port(transportOptions.Port))
			natPinger = traversal.NewNoopPinger()
		} else {
			portPool = port.NewPool()
			natPinger = di.NATPinger
		}

		manager := openvpn_service.NewManager(
			nodeOptions,
			transportOptions,
			loc.Country,
			di.IPResolver,
			di.ServiceSessionStorage,
			di.NATService,
			natPinger,
			di.NATTracker,
			portPool,
			di.EventBus,
			di.PortMapper,
			di.ServiceFirewall,
		)
		return manager, proposal, nil
	}
	di.ServiceRegistry.Register(service_openvpn.ServiceType, createService)
}

func (di *Dependencies) bootstrapServiceNoop(nodeOptions node.Options) {
	di.ServiceRegistry.Register(
		service_noop.ServiceType,
		func(serviceOptions service.Options) (service.Service, market.ServiceProposal, error) {
			loc, err := di.LocationResolver.DetectLocation()
			if err != nil {
				return nil, market.ServiceProposal{}, err
			}

			return service_noop.NewManager(), service_noop.GetProposal(loc), nil
		},
	)
}

func (di *Dependencies) bootstrapProviderRegistrar(nodeOptions node.Options) error {
	if nodeOptions.MobileConsumer {
		return nil
	}

	cfg := registry.ProviderRegistrarConfig{
		MaxRetries:          nodeOptions.Transactor.ProviderMaxRegistrationAttempts,
		Stake:               nodeOptions.Transactor.ProviderRegistrationStake,
		DelayBetweenRetries: nodeOptions.Transactor.ProviderRegistrationRetryDelay,
		AccountantAddress:   common.HexToAddress(nodeOptions.Accountant.AccountantID),
		RegistryAddress:     common.HexToAddress(nodeOptions.Transactor.RegistryAddress),
	}
	di.ProviderRegistrar = registry.NewProviderRegistrar(di.Transactor, di.IdentityRegistry, cfg)
	return di.ProviderRegistrar.Subscribe(di.EventBus)
}

func (di *Dependencies) bootstrapAccountantPromiseSettler(nodeOptions node.Options) error {
	if nodeOptions.MobileConsumer {
		di.AccountantPromiseSettler = &pingpong_noop.NoopAccountantPromiseSettler{}
		return nil
	}

	di.AccountantPromiseSettler = pingpong.NewAccountantPromiseSettler(
		di.EventBus,
		di.Transactor,
		di.AccountantPromiseStorage,
		di.BCHelper,
		di.IdentityRegistry,
		di.Keystore,
		pingpong.AccountantPromiseSettlerConfig{
			AccountantAddress:    common.HexToAddress(nodeOptions.Accountant.AccountantID),
			Threshold:            nodeOptions.Payments.AccountantPromiseSettlingThreshold,
			MaxWaitForSettlement: nodeOptions.Payments.SettlementTimeout,
		},
	)
	return di.AccountantPromiseSettler.Subscribe()
}

// bootstrapServiceComponents initiates ServicesManager dependency
func (di *Dependencies) bootstrapServiceComponents(nodeOptions node.Options, servicesOptions config.ServicesOptions) error {
	di.NATService = nat.NewService()
	if err := di.NATService.Enable(); err != nil {
		log.Warn().Err(err).Msg("Failed to enable NAT forwarding")
	}
	di.ServiceRegistry = service.NewRegistry()

	storage := session.NewEventBasedStorage(di.EventBus, session.NewStorageMemory())
	if err := storage.Subscribe(); err != nil {
		return errors.Wrap(err, "could not subscribe session to node events")
	}
	di.ServiceSessionStorage = storage

	di.PolicyOracle = policy.NewOracle(di.HTTPClient, servicesOptions.AccessPolicyAddress, servicesOptions.AccessPolicyFetchInterval)
	go di.PolicyOracle.Start()

	newDialogWaiter := func(providerID identity.Identity, serviceType string, policies *policy.Repository) (communication.DialogWaiter, error) {
		return nats_dialog.NewDialogWaiter(
			di.BrokerConnection,
			fmt.Sprintf("%v.%v", providerID.Address, serviceType),
			di.SignerFactory(providerID),
			policy.ValidateAllowedIdentity(policies),
		), nil
	}
	newP2PSessionHandler := func(proposal market.ServiceProposal, serviceID string, channel p2p.Channel) *session.Manager {
		paymentEngineFactory := pingpong.InvoiceFactoryCreator(nil,
			channel, nodeOptions.Payments.ProviderInvoiceFrequency,
			pingpong.PromiseWaitTimeout, di.ProviderInvoiceStorage,
			nodeOptions.Transactor.RegistryAddress,
			nodeOptions.Transactor.ChannelImplementation,
			pingpong.DefaultAccountantFailureCount,
			uint16(nodeOptions.Payments.MaxAllowedPaymentPercentile),
			di.BCHelper,
			di.EventBus,
			proposal,
			di.AccountantPromiseHandler,
		)
		return session.NewManager(
			proposal,
			di.ServiceSessionStorage,
			paymentEngineFactory,
			di.NATPinger,
			di.NATTracker,
			serviceID,
			di.EventBus,
			channel,
			session.DefaultConfig(),
		)
	}

	newDialogHandler := func(proposal market.ServiceProposal, configProvider session.ConfigProvider, serviceID string) (communication.DialogHandler, error) {
		sessionManagerFactory := newSessionManagerFactory(
			nodeOptions,
			proposal,
			di.ServiceSessionStorage,
			di.ProviderInvoiceStorage,
			di.NATPinger,
			di.NATTracker,
			serviceID,
			di.EventBus,
			di.BCHelper,
			di.AccountantPromiseHandler,
			di.HTTPClient,
			di.Keystore,
		)

		return session.NewDialogHandler(
			sessionManagerFactory,
			configProvider,
			identity.FromAddress(proposal.ProviderID),
			connectivity.NewStatusSubscriber(di.SessionConnectivityStatusStorage),
		), nil
	}

	di.ServicesManager = service.NewManager(
		di.ServiceRegistry,
		newDialogWaiter,
		newDialogHandler,
		di.DiscoveryFactory,
		di.EventBus,
		di.PolicyOracle,
		di.P2PListener,
		newP2PSessionHandler,
		di.SessionConnectivityStatusStorage,
	)

	serviceCleaner := service.Cleaner{SessionStorage: di.ServiceSessionStorage}
	if err := di.EventBus.Subscribe(servicestate.AppTopicServiceStatus, serviceCleaner.HandleServiceStatus); err != nil {
		log.Error().Msg("Failed to subscribe service cleaner")
	}

	return nil
}

func (di *Dependencies) registerConnections(nodeOptions node.Options) {
	if nodeOptions.MobileConsumer {
		di.registerNoopConnection()
		return
	}

	di.registerOpenvpnConnection(nodeOptions)
	di.registerNoopConnection()
	di.registerWireguardConnection(nodeOptions)
}

func (di *Dependencies) registerWireguardConnection(nodeOptions node.Options) {
	wireguard.Bootstrap()
	dnsManager := wireguard_connection.NewDNSManager()
	handshakeWaiter := wireguard_connection.NewHandshakeWaiter()
	endpointFactory := func() (wireguard.ConnectionEndpoint, error) {
		resourceAllocator := resources.NewAllocator(nil, wireguard_service.DefaultOptions.Subnet)
		return endpoint.NewConnectionEndpoint(resourceAllocator)
	}
	connFactory := func() (connection.Connection, error) {
		opts := wireguard_connection.Options{
			DNSConfigDir:     nodeOptions.Directories.Config,
			HandshakeTimeout: 1 * time.Minute,
		}
		return wireguard_connection.NewConnection(opts, di.IPResolver, di.NATPinger, endpointFactory, dnsManager, handshakeWaiter)
	}
	di.ConnectionRegistry.Register(wireguard.ServiceType, connFactory)
}

func (di *Dependencies) bootstrapUIServer(options node.Options) {
	if options.UI.UIEnabled {
		di.UIServer = ui.NewServer(options.BindAddress, options.UI.UIPort, options.TequilapiPort, di.JWTAuthenticator, di.HTTPClient)
		return
	}

	di.UIServer = uinoop.NewServer()
}

func (di *Dependencies) bootstrapMMN(options node.Options) {
	if !options.MMN.Enabled {
		return
	}

	collector := mmn.NewCollector(di.IPResolver)
	if err := collector.CollectEnvironmentInformation(); err != nil {
		log.Error().Msgf("Failed to collect environment information for MMN: %v", err)
		return
	}

	client := mmn.NewClient(di.HTTPClient, options.MMN.Address, di.SignerFactory)
	m := mmn.NewMMN(collector, client)

	if err := m.Subscribe(di.EventBus); err != nil {
		log.Error().Msgf("Failed to subscribe to events for MMN: %v", err)
	}
}
