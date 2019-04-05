// Package starlight exposes a payment channel agent on the Stellar network.
package starlight

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "github.com/coreos/bbolt"
	b "github.com/stellar/go/build"
	"github.com/stellar/go/clients/horizon"
	"github.com/stellar/go/network"
	"github.com/stellar/go/xdr"
	"golang.org/x/crypto/bcrypt"

	"github.com/interstellar/starlight/errors"
	"github.com/interstellar/starlight/net"
	"github.com/interstellar/starlight/starlight/db"
	"github.com/interstellar/starlight/starlight/fsm"
	"github.com/interstellar/starlight/starlight/internal/message"
	"github.com/interstellar/starlight/starlight/internal/update"
	"github.com/interstellar/starlight/starlight/key"
	"github.com/interstellar/starlight/starlight/taskbasket"
	"github.com/interstellar/starlight/worizon"
	"github.com/interstellar/starlight/worizon/xlm"
)

// An Agent acts on behalf of the user to open, close,
// maintain, and use payment channels.
// Its methods are safe to call concurrently.
// Methods 'Do*' initiate channel operations.
//
// An Agent serializes all its state changes
// and stores them in a database as Update records.
// Methods Wait and Updates provide synchronization
// and access (respectively) for updates.
type Agent struct {
	// An Agent has three levels of readiness:
	//
	//   1. brand new, not ready at all
	//   2. configured, but account not created yet
	//   3. fully ready, account created & funded
	//
	// The conventional way to distinguish these is by checking
	// the database for the presence of the Horizon URL and the
	// primary account's sequence number. Helper functions
	// isReadyConfigured and isReadyFunded do these checks.

	once    sync.Once // build handler
	handler http.Handler

	evcond sync.Cond

	// This is the root context object,
	// derived from the context passed to StartAgent.
	// It is used to create child contexts when starting new channels.
	rootCtx context.Context

	// This is the cancel function corresponding to rootCtx.
	rootCancel context.CancelFunc

	// Secret-key entropy seed; can be nil, see Authenticate.
	// Used to generate account keypairs.
	//
	// When seed is nil, there are many FSM inputs we can't handle.
	// We attempt to handle all inputs regardless, and if there's a
	// problem, such as seed being nil, we roll back the database
	// transaction. If the update was the result of a peer message,
	// we return an error to the peer, which will resend its message
	// later. Eventually, the local user will supply the password to
	// decrypt the seed, and then we'll be able to handle resent
	// messages (as well as all new inputs).
	seed []byte // write-once; synchronized with db.Update

	// Horizon client wrapper.
	wclient *worizon.Client

	// HTTP client used for agent requests. Treated as immutable state
	// after agent creation.
	httpclient http.Client

	tb *taskbasket.TB

	wg *sync.WaitGroup

	db *bolt.DB // doubles as a mutex for the fields in this struct

	// Channel to indicate when testnet faucet funds returns successfully
	wallet chan struct{}

	// Maps Starlight channel IDs to cancellation functions.
	// Call the cancellation function to stop the goroutines associated with the channel.
	cancelers map[string]context.CancelFunc

	// acctsReady maps channel IDs to a channel that indicates
	// whether or not the accounts have been successfully created
	// and are ready to be streamed from Horizon.
	acctsReady map[string]chan struct{}

	// These fields are used for logging.
	// They should be set once during initialization and not changed.
	// As such they may be accessed without holding the db mutex.
	name  string
	debug bool
}

// Config has user-facing, primary options for the Starlight agent
type Config struct {
	Username string `json:",omitempty"`
	Password string `json:",omitempty"`
	// WARNING: this software is not compatible with Stellar mainnet.
	HorizonURL string `json:",omitempty"`

	// OldPassword is required from the client in ConfigEdit
	// when changing the password.
	// It's never included in Updates.
	OldPassword string `json:",omitempty"`

	MaxRoundDurMins   int64      `json:",omitempty"`
	FinalityDelayMins int64      `json:",omitempty"`
	ChannelFeerate    xlm.Amount `json:",omitempty"`
	HostFeerate       xlm.Amount `json:",omitempty"`

	// KeepAlive, if set, indicates whether or not the agent will
	// send 0-value keep-alive payments on its channels
	KeepAlive *bool `json:",omitempty"`

	// Public indicates whether the agent is running on a
	// publicly-accessible URL. If the agent is public, then it is
	// able to propose and receive incoming channel requests.
	// Private agents can only propose channels
	// and see incoming messages on their local network.
	Public bool
}

const (
	tbBucket    = "tasks"
	baseReserve = 500 * xlm.Millilumen
)

// StartAgent starts an agent
// using the bucket "agent" in db for storage
// and returns it.
func StartAgent(ctx context.Context, boltDB *bolt.DB) (*Agent, error) {
	ctx, cancel := context.WithCancel(ctx)

	g := &Agent{
		db:         boltDB,
		cancelers:  make(map[string]context.CancelFunc),
		wg:         new(sync.WaitGroup),
		rootCtx:    ctx,
		rootCancel: cancel,
		wallet:     make(chan struct{}),
		acctsReady: make(map[string]chan struct{}),
		wclient:    new(worizon.Client),
	}

	g.evcond.L = new(sync.Mutex)

	err := db.Update(boltDB, func(root *db.Root) error { return g.start(root) })
	if err != nil {
		return nil, err
	}

	return g, nil
}

// Must be called from within an update transaction.
func (g *Agent) start(root *db.Root) error {
	if !g.isReadyConfigured(root) {
		return nil
	}
	if g.isReadyFunded(root) {
		close(g.wallet)
	} else {
		primaryAcct := *root.Agent().PrimaryAcct()
		g.allez(func() { g.getTestnetFaucetFunds(primaryAcct) }, "getTestnetFaucetFunds")
	}

	// WARNING: this software is not compatible with Stellar mainnet.
	g.wclient.SetURL(root.Agent().Config().HorizonURL())

	chans := root.Agent().Channels()

	var chanIDs []string
	err := chans.Bucket().ForEach(func(chanID, _ []byte) error {
		chanIDs = append(chanIDs, string(chanID))
		return nil
	})
	if err != nil {
		return err
	}

	for _, chanID := range chanIDs {
		err := g.startChannel(root, chanID)
		if err != nil {
			return err
		}
	}

	primaryAcct := root.Agent().PrimaryAcct().Address()
	w := root.Agent().Wallet()

	g.allez(func() { g.watchWalletAcct(primaryAcct, horizon.Cursor(w.Cursor)) }, "watchWalletAcct")

	tb, err := taskbasket.NewTx(g.rootCtx, root.Tx(), g.db, []byte(tbBucket), tbCodec{g: g})
	if err != nil {
		return err
	}
	g.tb = tb

	g.allez(func() { g.tb.Run(g.rootCtx) }, "taskbasket")

	return nil
}

// Close releases resources associated with the Agent.
// It does not wait for its subordinate goroutines to exit.
func (g *Agent) Close() {
	g.rootCancel()
}

// CloseWait releases resources associated with the Agent.
// It waits for its subordinate goroutines to exit.
func (g *Agent) CloseWait() {
	g.Close()
	g.wg.Wait()
}

// allez launches f as a goroutine, tracking it in the agent's WaitGroup.
func (g *Agent) allez(f func(), desc string) {
	g.wg.Add(1)
	go func() {
		g.debugf("%s starting", desc)
		f()
		g.debugf("%s finished", desc)
		g.wg.Done()
	}()
}

// ConfigInit sets g's configuration,
// generates a private key for the wallet,
// and performs any other necessary setup steps,
// such as obtaining free testnet lumens.
// It is an error if g has already been configured.
func (g *Agent) ConfigInit(c *Config, hostURL string) error {
	err := g.wclient.ValidateTestnetURL(c.HorizonURL)
	if err != nil {
		return err
	}

	return db.Update(g.db, func(root *db.Root) error {
		if g.isReadyConfigured(root) {
			return errAlreadyConfigured
		}

		g.seed = make([]byte, 32)
		randRead(g.seed)
		k := key.DeriveAccountPrimary(g.seed)
		primaryAcct := fsm.AccountID(key.PublicKeyXDR(k))

		if len(c.Password) > 72 {
			return errors.Wrap(errInvalidPassword, "too long (max 72 chars)") // bcrypt limit
		}
		if c.Password == "" {
			return errors.Wrap(errInvalidPassword, "empty password")
		}
		if !validateUsername(c.Username) {
			return errInvalidUsername
		}
		if c.MaxRoundDurMins < 0 {
			return errors.Wrap(errInvalidInput, "negative max round duration")
		}
		if c.FinalityDelayMins < 0 {
			return errors.Wrap(errInvalidInput, "negative finality delay")
		}
		if c.ChannelFeerate < 0 {
			return errors.Wrap(errInvalidInput, "negative channel feerate")
		}
		if c.HostFeerate < 0 {
			return errors.Wrap(errInvalidInput, "negative host feerate")
		}
		digest, err := bcrypt.GenerateFromPassword([]byte(c.Password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		root.Agent().Config().PutUsername(c.Username)
		root.Agent().Config().PutPwType("bcrypt")
		root.Agent().Config().PutPwHash(digest[:])
		root.Agent().Config().PutHorizonURL(c.HorizonURL)
		root.Agent().PutReady(true)
		root.Agent().PutEncryptedSeed(sealBox(g.seed, []byte(c.Password)))
		root.Agent().PutNextKeypathIndex(1)
		root.Agent().PutPrimaryAcct(&primaryAcct)
		if c.MaxRoundDurMins == 0 {
			c.MaxRoundDurMins = defaultMaxRoundDurMins
		}
		if c.FinalityDelayMins == 0 {
			c.FinalityDelayMins = defaultFinalityDelayMins
		}
		if c.ChannelFeerate == 0 {
			c.ChannelFeerate = defaultChannelFeerate
		}
		if c.HostFeerate == 0 {
			c.HostFeerate = defaultHostFeerate
		}
		if c.KeepAlive == nil {
			c.KeepAlive = new(bool)
			*c.KeepAlive = true
		}
		root.Agent().Config().PutMaxRoundDurMins(c.MaxRoundDurMins)
		root.Agent().Config().PutFinalityDelayMins(c.FinalityDelayMins)
		root.Agent().Config().PutChannelFeerate(int64(c.ChannelFeerate))
		root.Agent().Config().PutHostFeerate(int64(c.HostFeerate))
		root.Agent().Config().PutKeepAlive(*c.KeepAlive)
		root.Agent().Config().PutPublic(c.Public)

		// TODO(vniu): add tests for setting wallet address
		w := &fsm.WalletAcct{
			NativeBalance: xlm.Amount(0),
			Seqnum:        0,
			Cursor:        "",
			Address:       c.Username + "*" + hostURL,
			Balances:      map[string]fsm.Balance{},
		}
		root.Agent().PutWallet(w)
		// WARNING: this software is not compatible with Stellar mainnet.
		g.wclient.SetURL(c.HorizonURL)
		g.putUpdate(root, &Update{
			Type: update.InitType,
			Config: &update.Config{
				Username:          c.Username,
				Password:          "[redacted]",
				HorizonURL:        c.HorizonURL,
				MaxRoundDurMins:   c.MaxRoundDurMins,
				FinalityDelayMins: c.FinalityDelayMins,
				ChannelFeerate:    c.ChannelFeerate,
				HostFeerate:       c.HostFeerate,
				KeepAlive:         *c.KeepAlive,
			},
			Account: &update.Account{
				ID:      primaryAcct.Address(),
				Balance: 0,
			},
		})

		return g.start(root)
	})
}

// ConfigEdit edits g's configuration.
// Only Password and HorizonURL can be changed;
// attempting to change another field is an error.
func (g *Agent) ConfigEdit(c *Config) error {
	// Username and KeepAlive payments are not editable
	if c.Username != "" || c.KeepAlive != nil {
		return errInvalidEdit
	}

	// Check if config is empty
	c1 := *c
	c1.OldPassword = ""
	if c1 == (Config{}) {
		return errEmptyConfigEdit
	}
	if len(c.Password) > 72 {
		return errors.Wrap(errInvalidPassword, "too long (max 72 chars)") // bcrypt limit
	}
	if c.HorizonURL != "" {
		err := g.wclient.ValidateTestnetURL(c.HorizonURL)
		if err != nil {
			return err
		}
	}
	if c.MaxRoundDurMins < 0 {
		return errors.Wrap(errInvalidInput, "negative max round duration")
	}
	if c.FinalityDelayMins < 0 {
		return errors.Wrap(errInvalidInput, "negative finality delay")
	}
	if c.ChannelFeerate < 0 {
		return errors.Wrap(errInvalidInput, "negative channel feerate")
	}
	if c.HostFeerate < 0 {
		return errors.Wrap(errInvalidInput, "negative host feerate")
	}

	return db.Update(g.db, func(root *db.Root) error {
		if !g.isReadyConfigured(root) {
			return errNotConfigured
		}
		if c.Password != "" {
			if root.Agent().Config().PwType() != "bcrypt" {
				return nil
			}
			digest := root.Agent().Config().PwHash()
			err := bcrypt.CompareHashAndPassword(digest, []byte(c.OldPassword))
			if err != nil {
				return errors.Sub(errPasswordsDontMatch, err)
			}

			digest, err = bcrypt.GenerateFromPassword([]byte(c.Password), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			root.Agent().Config().PutPwType("bcrypt")
			root.Agent().Config().PutPwHash(digest[:])
			root.Agent().PutEncryptedSeed(sealBox(g.seed, []byte(c.Password)))
		}
		// WARNING: this software is not compatible with Stellar mainnet.
		if c.HorizonURL != "" {
			root.Agent().Config().PutHorizonURL(c.HorizonURL)
			g.wclient.SetURL(c.HorizonURL)
		}
		if c.MaxRoundDurMins != 0 {
			root.Agent().Config().PutMaxRoundDurMins(c.MaxRoundDurMins)
		}
		if c.FinalityDelayMins != 0 {
			root.Agent().Config().PutFinalityDelayMins(c.FinalityDelayMins)
		}
		if c.ChannelFeerate != 0 {
			root.Agent().Config().PutChannelFeerate(int64(c.ChannelFeerate))
		}
		if c.HostFeerate != 0 {
			root.Agent().Config().PutHostFeerate(int64(c.HostFeerate))
		}
		g.putUpdate(root, &Update{
			Type: update.ConfigType,
			Config: &update.Config{
				Username:          c.Username,
				Password:          "[redacted]",
				HorizonURL:        c.HorizonURL,
				MaxRoundDurMins:   c.MaxRoundDurMins,
				FinalityDelayMins: c.FinalityDelayMins,
				ChannelFeerate:    c.ChannelFeerate,
				HostFeerate:       c.HostFeerate,
			},
		})
		return nil
	})
}

// AddAsset creates a trustline for a non-native Asset.
func (g *Agent) AddAsset(assetCode, issuer string) error {
	if assetCode == "" {
		return errEmptyAsset
	}
	if issuer == "" {
		return errEmptyIssuer
	}
	return db.Update(g.db, func(root *db.Root) error {
		if !root.Agent().Ready() {
			return errAgentClosing
		}
		w := root.Agent().Wallet()
		hostFeerate := xlm.Amount(root.Agent().Config().HostFeerate())
		if w.NativeBalance < (hostFeerate + baseReserve) {
			return errors.Wrap(errInsufficientBalance, "fees and reserve to add non-native asset")
		}
		var issuerAccountID xdr.AccountId
		err := issuerAccountID.SetAddress(issuer)
		if err != nil {
			return errors.Sub(errInvalidAddress, err)
		}
		var asset xdr.Asset
		err = asset.SetCredit(assetCode, issuerAccountID)
		if err != nil {
			return errors.Sub(errInvalidAsset, err)
		}
		w.Balances[asset.String()] = fsm.Balance{
			Asset:      asset,
			Amount:     0,
			Pending:    true,
			Authorized: false,
		}
		w.NativeBalance -= (baseReserve + hostFeerate)
		w.Reserve += baseReserve
		w.Seqnum++
		root.Agent().PutWallet(w)

		btx, err := b.Transaction(
			b.Network{Passphrase: g.passphrase(root)},
			b.SourceAccount{AddressOrSeed: w.Address},
			b.Sequence{Sequence: uint64(w.Seqnum)},
			b.Trust(assetCode, issuer),
		)
		if err != nil {
			return err
		}
		k := key.DeriveAccountPrimary(g.seed)
		env, err := btx.Sign(k.Seed())
		if err != nil {
			return err
		}
		time := g.wclient.Now()
		g.putUpdate(root, &Update{
			Type: update.AccountType,
			Account: &update.Account{
				ID:       w.Address,
				Balances: w.Balances,
			},
			InputCommand: &fsm.Command{
				Name:      fsm.AddAsset,
				Time:      time,
				AssetCode: assetCode,
				Issuer:    issuer,
			},
			PendingSequence: strconv.FormatInt(int64(w.Seqnum), 10),
		})
		return g.addTxTask(root.Tx(), walletBucket, *env.E)
	})
}

// RemoveAsset deletes a trustline for a non-native Asset.
func (g *Agent) RemoveAsset(assetCode, issuer string) error {
	if assetCode == "" {
		return errEmptyAsset
	}
	if issuer == "" {
		return errEmptyIssuer
	}
	return db.Update(g.db, func(root *db.Root) error {
		if !root.Agent().Ready() {
			return errAgentClosing
		}
		w := root.Agent().Wallet()
		hostFeerate := xlm.Amount(root.Agent().Config().HostFeerate())
		if w.NativeBalance < hostFeerate {
			return errors.Wrap(errInsufficientBalance, "fees to remove non-native asset")
		}
		var issuerAccountID xdr.AccountId
		err := issuerAccountID.SetAddress(issuer)
		if err != nil {
			return errors.Sub(errInvalidAddress, err)
		}
		var asset xdr.Asset
		err = asset.SetCredit(assetCode, issuerAccountID)
		if err != nil {
			return errors.Sub(errInvalidAsset, err)
		}
		var (
			currBalance fsm.Balance
			ok          bool
		)
		if currBalance, ok = w.Balances[asset.String()]; !ok {
			return errInvalidAsset
		}
		if currBalance.Amount != 0 {
			return errors.New("cannot remove trustline with nonzero balance")
		}
		currBalance.Authorized = false
		w.Balances[asset.String()] = currBalance
		w.NativeBalance -= hostFeerate
		w.Seqnum++
		root.Agent().PutWallet(w)
		btx, err := b.Transaction(
			b.Network{Passphrase: g.passphrase(root)},
			b.SourceAccount{AddressOrSeed: w.Address},
			b.Sequence{Sequence: uint64(w.Seqnum)},
			b.RemoveTrust(assetCode, issuer),
		)
		if err != nil {
			return err
		}
		k := key.DeriveAccountPrimary(g.seed)
		env, err := btx.Sign(k.Seed())
		if err != nil {
			return err
		}
		time := g.wclient.Now()
		g.putUpdate(root, &Update{
			Type: update.AccountType,
			Account: &update.Account{
				ID:       w.Address,
				Balances: w.Balances,
			},
			InputCommand: &fsm.Command{
				Name:      fsm.RemoveAsset,
				Time:      time,
				AssetCode: assetCode,
				Issuer:    issuer,
			},
			PendingSequence: strconv.FormatInt(int64(w.Seqnum), 10),
		})
		return g.addTxTask(root.Tx(), walletBucket, *env.E)
	})
}

// Configured returns whether ConfigInit has been called on g.
func (g *Agent) Configured() bool {
	var ok bool
	db.View(g.db, func(root *db.Root) error {
		ok = g.isReadyConfigured(root)
		return nil
	})
	return ok
}

func (g *Agent) isReadyConfigured(root *db.Root) bool {
	return root.Agent().Config().HorizonURL() != ""
}

func (g *Agent) isReadyFunded(root *db.Root) bool {
	return root.Agent().Wallet().Seqnum > 0
}

// Function watchWalletAcct runs in its own goroutine waiting for creation of the wallet account,
// and payments or merges into it.
// When such transactions hit the ledger,
// it reports an *Update back for the client to consume.
func (g *Agent) watchWalletAcct(acctID string, cursor horizon.Cursor) {
	// Wait until getTestnetFaucetFunds returns successfully

	select {
	// Block until accounts are ready to be watched
	case <-g.wallet:
		break
	case <-g.rootCtx.Done():
		return
	}

	err := g.wclient.StreamTxs(g.rootCtx, acctID, cursor, func(htx worizon.Transaction) error {
		InputTx, err := worizon.NewTx(&htx)
		if err != nil {
			return err
		}
		if InputTx.Result.Result.Code != xdr.TransactionResultCodeTxSuccess {
			// Ignore failed txs.
			return nil
		}
		db.Update(g.db, func(root *db.Root) error {
			// log succcessfully sent transactions
			if InputTx.Env.Tx.SourceAccount.Address() == acctID {
				w := root.Agent().Wallet()
				w.Cursor = htx.PT
				root.Agent().PutWallet(w)
				g.putUpdate(root, &Update{
					Type:    update.TxSuccessType,
					InputTx: InputTx,
				})
			}
			for index, op := range InputTx.Env.Tx.Operations {
				switch op.Body.Type {
				case xdr.OperationTypeCreateAccount:
					// watch for escrow accounts being created, close the acctReady channel
					createAccount := op.Body.CreateAccountOp
					createAccountAddr := createAccount.Destination.Address()
					if acctReady, ok := g.acctsReady[createAccountAddr]; ok {
						close(acctReady)
						delete(g.acctsReady, createAccountAddr)
					}

					if createAccountAddr != acctID {
						continue
					}

					// compute the initial sequence number of the account
					// it's the ledger number of the transaction that created it, shifted left 32 bits
					seqnum := xdr.SequenceNumber(uint64(htx.Ledger) << 32)

					w := root.Agent().Wallet()
					hostFeerate := root.Agent().Config().HostFeerate()
					w.NativeBalance = xlm.Amount(createAccount.StartingBalance) - xlm.Amount(hostFeerate) - 2*baseReserve
					w.Reserve = 2 * baseReserve
					w.Seqnum = seqnum
					w.Cursor = htx.PT
					root.Agent().PutWallet(w)
					g.putUpdate(root, &Update{
						Type: update.AccountType,
						Account: &update.Account{
							ID:       acctID,
							Balance:  uint64(w.NativeBalance),
							Balances: w.Balances,
							Reserve:  uint64(w.Reserve),
						},
						InputTx: InputTx,
						OpIndex: index,
					})
					if root.Agent().Config().Public() {
						// Set account home domain
						domain := w.Address[strings.Index(w.Address, "*")+1:]
						w.Seqnum++
						tx, err := b.Transaction(
							b.Network{Passphrase: g.passphrase(root)},
							b.BaseFee{Amount: uint64(hostFeerate)},
							b.SourceAccount{AddressOrSeed: acctID},
							b.Sequence{Sequence: uint64(w.Seqnum)},
							b.SetOptions(
								b.SourceAccount{AddressOrSeed: acctID},
								b.HomeDomain(domain),
							),
						)
						if err != nil {
							return errors.Wrap(err, "building home domain tx")
						}
						k := key.DeriveAccountPrimary(g.seed)
						env, err := tx.Sign(k.Seed())
						if err != nil {
							return err
						}
						// create transaction to set options
						g.addTxTask(root.Tx(), walletBucket, *env.E)
					}

				case xdr.OperationTypePayment:
					paymentOp := op.Body.PaymentOp
					if paymentOp.Destination.Address() != acctID {
						continue
					}
					w := root.Agent().Wallet()
					var asset xdr.Asset
					switch paymentOp.Asset.Type {
					case xdr.AssetTypeAssetTypeNative:
						w.NativeBalance += xlm.Amount(paymentOp.Amount)
					case xdr.AssetTypeAssetTypeCreditAlphanum4:
						if shortAsset, ok := paymentOp.Asset.GetAlphaNum4(); ok {
							// Since assets are treated as credits on the Stellar network,
							// payments of an asset back to the issuer disappear.
							if shortAsset.Issuer.Address() == acctID {
								continue
							}
							err = asset.SetCredit(string(shortAsset.AssetCode[:]), shortAsset.Issuer)
							if err != nil {
								return errors.Sub(err, errInvalidAsset)
							}
						}
					case xdr.AssetTypeAssetTypeCreditAlphanum12:
						if longAsset, ok := paymentOp.Asset.GetAlphaNum12(); ok {
							if longAsset.Issuer.Address() == acctID {
								continue
							}
							err = asset.SetCredit(string(longAsset.AssetCode[:]), longAsset.Issuer)
							if err != nil {
								return errors.Sub(err, errInvalidAsset)
							}
						}
					}
					if asset.Type != xdr.AssetTypeAssetTypeNative {
						assetStr := asset.String()
						var currBalance fsm.Balance
						if currBalance, ok := w.Balances[assetStr]; ok {
							currBalance.Amount += uint64(paymentOp.Amount)
						} else {
							currBalance = fsm.Balance{
								Asset:      asset,
								Amount:     uint64(paymentOp.Amount),
								Pending:    false,
								Authorized: true,
							}
						}
						w.Balances[assetStr] = currBalance
					}
					w.Cursor = htx.PT
					root.Agent().PutWallet(w)
					g.putUpdate(root, &Update{
						Type: update.AccountType,
						Account: &update.Account{
							ID:       acctID,
							Balance:  uint64(w.NativeBalance),
							Balances: w.Balances,
							Reserve:  uint64(w.Reserve),
						},
						InputTx: InputTx,
						OpIndex: index,
					})

				case xdr.OperationTypeAccountMerge:
					if op.SourceAccount.Address() == acctID {
						// Wipe the database
						root.DeleteAgent()
						// Publish update that the Agent has been reset
						g.putUpdate(root, &Update{
							Type: update.AccountType,
							Account: &update.Account{
								ID:      acctID,
								Balance: 0,
							},
							InputTx: InputTx,
							OpIndex: index,
						})
						// To open the Agent to being reconfigured, indicate
						// that the shutdown is complete and Agent is now
						// Ready to accept new commands again.
						root.Agent().PutReady(true)
					}
					if op.Body.Destination.Address() == acctID {
						// Note: account merge amounts are always in lumens.
						// See https://www.stellar.org/developers/guides/concepts/list-of-operations.html#account-merge.

						// If the tx is successful and InputTx.Env.Tx.Operations[index] is an account merge,
						// we can depend on (*InputTx.Result.Result.Results)[index].Tr being present and having an AccountMergeResult.
						mergeAmount := *(*InputTx.Result.Result.Results)[index].Tr.AccountMergeResult.SourceAccountBalance
						w := root.Agent().Wallet()
						w.NativeBalance += xlm.Amount(mergeAmount)
						w.Cursor = htx.PT
						root.Agent().PutWallet(w)

						g.putUpdate(root, &Update{
							Type: update.AccountType,
							Account: &update.Account{
								ID:       acctID,
								Balance:  uint64(w.NativeBalance),
								Balances: w.Balances,
								Reserve:  uint64(w.Reserve),
							},
							InputTx: InputTx,
							OpIndex: index,
						})
					}
				case xdr.OperationTypeChangeTrust:
					// AddAsset and RemoveAsset both have this operation.
					changeTrustOp, ok := op.Body.GetChangeTrustOp()
					if !ok {
						return errors.New("change trust op failed")
					}
					w := root.Agent().Wallet()
					if op.SourceAccount.Address() != w.Address {
						continue
					}
					if changeTrustOp.Limit == 0 { // RemoveAsset
						delete(w.Balances, changeTrustOp.Line.String())
						w.NativeBalance += baseReserve // unreserve base reserve
						w.Reserve -= baseReserve
					} else { // AddAsset
						var issuer xdr.AccountId
						switch changeTrustOp.Line.Type {
						case xdr.AssetTypeAssetTypeCreditAlphanum4:
							if shortAsset, ok := changeTrustOp.Line.GetAlphaNum4(); ok {
								issuer = shortAsset.Issuer
							}
						case xdr.AssetTypeAssetTypeCreditAlphanum12:
							if longAsset, ok := changeTrustOp.Line.GetAlphaNum12(); ok {
								issuer = longAsset.Issuer
							}
						default:
							return errors.New("native trustline not allowed")
						}
						account, err := g.wclient.LoadAccount(issuer.Address())
						if err != nil {
							return errors.Wrap(err, "getting issuer auth requirement")
						}
						authorized := !(account.Flags.AuthRequired)
						w.Balances[changeTrustOp.Line.String()] = fsm.Balance{
							Asset:      changeTrustOp.Line,
							Pending:    false,
							Authorized: authorized,
						}

					}
					root.Agent().PutWallet(w)
					g.putUpdate(root, &Update{
						Type: update.AccountType,
						Account: &update.Account{
							ID:       acctID,
							Balance:  uint64(w.NativeBalance),
							Balances: w.Balances,
							Reserve:  uint64(w.Reserve),
						},
						InputTx: InputTx,
						OpIndex: index,
					})
				case xdr.OperationTypeAllowTrust:
					allowTrustOp := op.Body.AllowTrustOp
					if allowTrustOp.Trustor.Address() != acctID {
						continue
					}
					var asset xdr.Asset
					switch allowTrustOp.Asset.Type {
					case xdr.AssetTypeAssetTypeCreditAlphanum4:
						assetCode, _ := allowTrustOp.Asset.GetAssetCode4()
						err = asset.SetCredit(string(assetCode[:]), *op.SourceAccount)
						if err != nil {
							return errors.Sub(err, errInvalidAsset)
						}
					case xdr.AssetTypeAssetTypeCreditAlphanum12:
						assetCode, _ := allowTrustOp.Asset.GetAssetCode12()
						err = asset.SetCredit(string(assetCode[:]), *op.SourceAccount)
						if err != nil {
							return errors.Sub(err, errInvalidAsset)
						}
					default:
						return errors.New("no native trustline allowed")
					}
					w := root.Agent().Wallet()
					assetStr := asset.String()
					var currBalance fsm.Balance
					if currBalance, ok := w.Balances[assetStr]; ok {
						currBalance.Authorized = allowTrustOp.Authorize
					} else {
						currBalance = fsm.Balance{
							Asset:      asset,
							Pending:    false,
							Authorized: allowTrustOp.Authorize,
						}
					}
					w.Balances[asset.String()] = currBalance
					root.Agent().PutWallet(w)
					g.putUpdate(root, &Update{
						Type: update.AccountType,
						Account: &update.Account{
							ID:       acctID,
							Balance:  uint64(w.NativeBalance),
							Balances: w.Balances,
						},
						InputTx: InputTx,
						OpIndex: index,
					})
				}
			}
			return nil
		})
		return nil
	})
	if err != nil {
		g.debugf("watching wallet-account txs: %s", err)
		g.mustDeauthenticate()
	}
}

func (g *Agent) getTestnetFaucetFunds(acctID fsm.AccountID) {
	// The faucet is not 100% reliable (it often times out),
	// so this tries indefinitely with backoff until success.
	backoff := &net.Backoff{Base: 100 * time.Millisecond}
	defer close(g.wallet)

	acctIDStr := acctID.Address()
	faucetURL := "https://friendbot.stellar.org/?addr=" + acctIDStr

	for counter := 0; ; counter++ {
		if counter == 1 {
			db.Update(g.db, func(root *db.Root) error {
				g.putUpdate(root, &Update{
					Type:    update.WarningType,
					Warning: "could not retrieve testnet faucet funds, will retry until successful",
				})
				return nil
			})
		}

		err := func() error {
			resp, err := g.httpclient.Get(faucetURL)
			if err != nil {
				return err
			}
			if resp.Body != nil {
				defer resp.Body.Close()
			}

			if resp.StatusCode/100 == 2 {
				return nil
			}

			var v struct {
				Detail      string
				ResultCodes json.RawMessage `json:"result_codes"`
			}
			jErr := json.NewDecoder(resp.Body).Decode(&v)
			if jErr != nil {
				return fmt.Errorf("bad http status %d from faucet at %s", resp.StatusCode, faucetURL)
			}
			return fmt.Errorf("faucet at %s: %s (%s); http status %d", faucetURL, v.Detail, v.ResultCodes, resp.StatusCode)
		}()
		if err != nil {
			dur := backoff.Next()
			g.debugf("getting testnet funds for %s failed, will retry in %s: %s", acctIDStr, dur, err)
			g.sleep(dur)
			continue
		}
		return
	}
}

// Authenticate authenticates the given user name and password.
// If they're valid, it also decrypts the secret entropy seed
// if necessary, allowing private-key operations to proceed.
//
// It returns whether name and password are valid.
func (g *Agent) Authenticate(name, password string) bool {
	var ok bool
	var seed []byte
	if !validateUsername(name) {
		return false
	}
	db.View(g.db, func(root *db.Root) error {
		if !g.isReadyConfigured(root) {
			return nil
		}
		if name != root.Agent().Config().Username() {
			return nil
		}
		if root.Agent().Config().PwType() != "bcrypt" {
			return nil
		}
		digest := root.Agent().Config().PwHash()
		err := bcrypt.CompareHashAndPassword(digest, []byte(password))
		ok = err == nil
		seed = g.seed
		return nil
	})
	if ok && seed == nil {
		err := db.Update(g.db, func(root *db.Root) error {
			if g.seed != nil {
				return nil // already decrypted
			}
			encseed := root.Agent().EncryptedSeed()
			g.seed = openBox(encseed, []byte(password))
			return nil
		})
		if err != nil {
			panic(err)
		}
	}
	return ok
}

func (g *Agent) mustDeauthenticate() {
	err := db.Update(g.db, func(root *db.Root) error {
		if g.seed != nil {
			g.logf("entering watchtower mode")
			g.seed = nil
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
}

const (
	defaultMaxRoundDurMins   = 60
	defaultFinalityDelayMins = 60
	defaultChannelFeerate    = 10 * xlm.Millilumen
	defaultHostFeerate       = 100 * xlm.Stroop
)

// checkChannelUnique checks if there exists a channel between two parties with
// account IDS a and b. If so, it returns the channel ID and an error.
func (g *Agent) checkChannelUnique(a, b string) ([]byte, error) {
	var chanID []byte
	err := db.View(g.db, func(root *db.Root) error {
		chans := root.Agent().Channels()
		return chans.Bucket().ForEach(func(curChanID, _ []byte) error {
			c := chans.Get(curChanID)
			p, q := c.HostAcct.Address(), c.GuestAcct.Address()
			if (a == p && b == q) || (a == q && b == p) {
				chanID = curChanID
				return errors.Wrapf(errExists, "between host %s and guest %s", p, q)
			}
			return nil
		})
	})
	return chanID, err
}

// DoCreateChannel creates a channel between the agent host and the guest
// specified at guestFedAddr, funding the channel with hostAmount
func (g *Agent) DoCreateChannel(guestFedAddr string, hostAmount xlm.Amount) (*fsm.Channel, error) {
	if guestFedAddr == "" {
		return nil, errEmptyAddress
	}
	if hostAmount == 0 {
		return nil, errEmptyAmount
	}
	// TODO(debnil): Distinguish account string and federation server address better, i.e. using type aliases for string.
	var hostAcctStr string
	db.View(g.db, func(root *db.Root) error {
		hostAcctStr = root.Agent().PrimaryAcct().Address()
		return nil
	})

	guestAcctStr, starlightURL, err := g.FindAccount(guestFedAddr)
	if err != nil {
		return nil, errors.Wrapf(err, "finding account %s", guestFedAddr)
	}
	if guestAcctStr == hostAcctStr {
		return nil, errAcctsSame
	}
	_, err = g.checkChannelUnique(hostAcctStr, guestAcctStr)
	if err != nil {
		return nil, err
	}

	var ch *fsm.Channel
	err = db.Update(g.db, func(root *db.Root) error {
		if !root.Agent().Ready() {
			return errAgentClosing
		}
		if !g.isReadyFunded(root) {
			return errNotFunded
		}

		w := root.Agent().Wallet()
		w.Seqnum += 3

		// Local node is the host.
		// Remote node is the guest.

		var guestAcct fsm.AccountID
		err := guestAcct.SetAddress(guestAcctStr)
		if err != nil {
			err = errors.Sub(errInvalidAddress, err)
			return errors.Wrap(err, "guest address", guestAcctStr)
		}

		channelKeyIndex := nextChannelKeyIndex(root.Agent(), 3)
		channelKeyPair := key.DeriveAccount(g.seed, channelKeyIndex)
		channelID := channelKeyPair.Address()

		var escrowAcct fsm.AccountID
		err = escrowAcct.SetAddress(channelKeyPair.Address())
		if err != nil {
			return errors.Wrap(err, "setting escrow address", channelKeyPair.Address())
		}

		firstThrowawayKeyPair := key.DeriveAccount(g.seed, channelKeyIndex+1)
		var hostRatchetAcct fsm.AccountID
		err = hostRatchetAcct.SetAddress(firstThrowawayKeyPair.Address())
		if err != nil {
			return errors.Wrap(err, "setting host ratchet address", firstThrowawayKeyPair.Address())
		}

		secondThrowawayKeyPair := key.DeriveAccount(g.seed, channelKeyIndex+2)
		var guestRatchetAcct fsm.AccountID
		err = guestRatchetAcct.SetAddress(secondThrowawayKeyPair.Address())
		if err != nil {
			return errors.Wrap(err, "setting guest ratchet address", secondThrowawayKeyPair.Address())
		}

		fundingTime := g.wclient.Now()

		if ch = g.getChannel(root, channelID); ch.State != fsm.Start {
			return errors.Wrap(errExists, string(channelID))
		}

		ch = &fsm.Channel{
			ID:                  channelID,
			Role:                fsm.Host,
			HostAmount:          hostAmount,
			CounterpartyAddress: guestFedAddr,
			RemoteURL:           starlightURL,
			Passphrase:          g.passphrase(root),
			MaxRoundDuration:    time.Duration(root.Agent().Config().MaxRoundDurMins()) * time.Minute,
			FinalityDelay:       time.Duration(root.Agent().Config().FinalityDelayMins()) * time.Minute,
			ChannelFeerate:      xlm.Amount(root.Agent().Config().ChannelFeerate()),
			HostFeerate:         xlm.Amount(root.Agent().Config().HostFeerate()),
			FundingTime:         fundingTime,
			PaymentTime:         fundingTime,
			KeyIndex:            channelKeyIndex,
			GuestAcct:           guestAcct,
			EscrowAcct:          escrowAcct,
			HostRatchetAcct:     hostRatchetAcct,
			GuestRatchetAcct:    guestRatchetAcct,
			RoundNumber:         1,
		}
		err = ch.HostAcct.SetAddress(hostAcctStr)
		if err != nil {
			return errors.Wrap(err, "setting host address")
		}
		newBalance := w.NativeBalance - ch.SetupAndFundingReserveAmount()
		if newBalance < 0 {
			return errors.Wrap(errInsufficientBalance, w.NativeBalance.String())
		}
		w.NativeBalance = newBalance
		g.putChannel(root, channelID, ch)
		root.Agent().PutWallet(w)

		return g.doUpdateChannel(root, ch.ID, func(root *db.Root, updater *fsm.Updater, update *Update) error {
			c := &fsm.Command{
				Name:      fsm.CreateChannel,
				Amount:    ch.HostAmount,
				Recipient: guestFedAddr,
			}
			update.InputCommand = c
			return updater.Cmd(c)
		})
	})
	return ch, err
}

// DoWalletPay implements the wallet-pay command.
func (g *Agent) DoWalletPay(dest string, amount uint64, assetCode, issuer string) error {
	if dest == "" {
		return errEmptyAddress
	}
	if amount == 0 {
		return errEmptyAmount
	}
	if assetCode == "" && issuer != "" {
		return errEmptyAsset
	}
	if assetCode != "" && issuer == "" {
		return errEmptyIssuer
	}
	return db.Update(g.db, func(root *db.Root) error {
		if !root.Agent().Ready() {
			return errAgentClosing
		}
		var (
			paymentOp b.PaymentBuilder
			assetStr  string
		)
		w := root.Agent().Wallet()
		hostAcct := root.Agent().PrimaryAcct()
		hostFeerate := xlm.Amount(root.Agent().Config().HostFeerate())
		if assetCode != "" && issuer != "" {
			if w.NativeBalance <= hostFeerate {
				return errors.Wrap(errInsufficientBalance, "XLM balance for host fee")
			}
			var issuerAccountID xdr.AccountId
			err := issuerAccountID.SetAddress(issuer)
			if err != nil {
				return errors.Wrap(errors.Sub(err, errInvalidAddress), "invalid issuer account")
			}
			var asset xdr.Asset
			err = asset.SetCredit(assetCode, issuerAccountID)
			if err != nil {
				return errors.Sub(err, errInvalidAsset)
			}
			// Check if the wallet/host is issuing their own asset.
			// Else, check for an existing trustline.
			if issuer != hostAcct.Address() {
				assetStr = asset.String()
				if currBalance, ok := w.Balances[assetStr]; ok {
					if currBalance.Amount <= amount {
						return errors.Wrap(errInsufficientBalance, "asset amount for payment")
					}
					if !currBalance.Authorized {
						return errors.New(fmt.Sprintf("unauthorized trustline for %s", assetStr))
					}
					currBalance.Amount -= amount
					w.Balances[assetStr] = currBalance
				} else {
					return errors.Wrap(errInvalidAsset, fmt.Sprintf("no trustline exists for asset %s, issuer %s", assetCode, issuer))
				}
			}
			w.NativeBalance -= hostFeerate
			paymentOp = b.Payment(
				b.SourceAccount{AddressOrSeed: hostAcct.Address()},
				b.Destination{AddressOrSeed: dest},
				b.CreditAmount{
					Code:   assetCode,
					Issuer: issuer,
					Amount: string(amount),
				},
			)
		} else {
			if w.NativeBalance <= xlm.Amount(amount)+hostFeerate {
				return errors.Wrap(errInsufficientBalance, "XLM amount for payment and fees")
			}
			w.NativeBalance -= (xlm.Amount(amount) + hostFeerate)
			paymentOp = b.Payment(
				b.SourceAccount{AddressOrSeed: hostAcct.Address()},
				b.Destination{AddressOrSeed: dest},
				b.NativeAmount{Amount: xlm.Amount(amount).HorizonString()},
			)
		}
		w.Seqnum++
		root.Agent().PutWallet(w)

		btx, err := b.Transaction(
			b.Network{Passphrase: g.passphrase(root)},
			b.SourceAccount{AddressOrSeed: hostAcct.Address()},
			b.Sequence{Sequence: uint64(w.Seqnum)},
			paymentOp,
		)
		if err != nil {
			return err
		}
		k := key.DeriveAccountPrimary(g.seed)
		env, err := btx.Sign(k.Seed())
		if err != nil {
			return err
		}
		time := g.wclient.Now()
		g.putUpdate(root, &Update{
			Type: update.AccountType,
			Account: &update.Account{
				ID:       hostAcct.Address(),
				Balance:  uint64(w.NativeBalance),
				Balances: w.Balances,
				Reserve:  uint64(w.Reserve),
			},
			// TODO (debnil): Make Command.Amount uint, not XLM.
			InputCommand: &fsm.Command{
				Name:      fsm.Pay,
				Amount:    xlm.Amount(amount),
				Recipient: dest,
				Time:      time,
				AssetCode: assetCode,
				Issuer:    issuer,
			},
			InputLedgerTime: time,
			PendingSequence: strconv.FormatInt(int64(w.Seqnum), 10),
		})
		return g.addTxTask(root.Tx(), walletBucket, *env.E)
	})
}

func (g *Agent) addTxTask(tx *bolt.Tx, chanID string, e xdr.TransactionEnvelope) error {
	t := &TbTx{
		g:      g,
		ChanID: chanID,
		E:      e,
	}
	return g.tb.AddTx(tx, t)
}

func (g *Agent) addMsgTask(root *db.Root, c *fsm.Channel, msg *fsm.Message) error {
	if c.Role == fsm.Guest {
		g.putMessage(root, c, msg)
		c.LastMsgIndex = msg.MsgNum
		g.putChannel(root, c.ID, c)
		return nil
	}
	m := &TbMsg{
		g:         g,
		RemoteURL: c.RemoteURL,
		Msg:       *msg,
	}
	return g.tb.AddTx(root.Tx(), m)
}

func (g *Agent) putMessage(root *db.Root, c *fsm.Channel, msg *fsm.Message) {
	m := root.Agent().Messages().Get([]byte(c.ID))
	if m == nil {
		m = new(message.Message)
	}
	m.Add(msg, &msg.MsgNum)
	root.Agent().Messages().Put([]byte(c.ID), m)
	root.Tx().OnCommit(g.evcond.Broadcast)
}

// Messages returns all messages sent by the agent on
// channel chanID in the half-open interval [a, b).
// The returned slice will have length less than b-a
// if a or b is out of range.
func (g *Agent) Messages(chanID string, a, b uint64) []*fsm.Message {
	msgs := make([]*fsm.Message, 0)
	err := db.View(g.db, func(root *db.Root) error {
		m := root.Agent().Messages().GetByString(chanID)
		msgs = append(msgs, m.From(a, b)...)
		return nil
	})
	if err != nil {
		panic(err) // only errors here are bugs
	}
	return msgs
}

// WaitMsg blocks until a message with number i is available for the
// channel chanID
func (g *Agent) WaitMsg(ctx context.Context, chanID string, i uint64) {
	go func() {
		<-ctx.Done()
		g.evcond.Broadcast()
	}()
	g.evcond.L.Lock()
	defer g.evcond.L.Unlock()
	for lastMsgNum(g.db, chanID) < i && ctx.Err() == nil {
		g.evcond.Wait()
	}
}

func lastMsgNum(boltDB *bolt.DB, chanID string) (n uint64) {
	err := db.View(boltDB, func(root *db.Root) error {
		if m := root.Agent().Messages().GetByString(chanID); m != nil {
			n = m.LastSeqNum
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	return n
}

// DoCloseAccount will merge the agent's wallet account into the
// specified destination account. If the agent has any channels
// that are not closed, it will fail. While the agent is shutting
// down, it will not accept any other commands. If the transaction
// fails, the agent will alert the user and return to an Active
// state. Otherwise, the agent transitions to an empty initial
// state on merge success.
func (g *Agent) DoCloseAccount(dest string) error {
	return db.Update(g.db, func(root *db.Root) error {
		if !root.Agent().Ready() {
			return errAgentClosing
		}
		var chanIDs []string
		chans := root.Agent().Channels()
		err := chans.Bucket().ForEach(func(chanID, _ []byte) error {
			chanIDs = append(chanIDs, string(chanID))
			return nil
		})
		if err != nil {
			return err
		}
		for _, chanID := range chanIDs {
			ch := chans.Get([]byte(chanID))
			if ch.State != fsm.Closed {
				return errors.New(fmt.Sprintf("channel %s in non-closed state %s", string(chanID), ch.State))
			}
		}
		// Agent is closing, and not able to accept new requests.
		root.Agent().PutReady(false)
		hostAcct := root.Agent().PrimaryAcct()
		closeAccountBuilder, err := b.Transaction(
			b.Network{Passphrase: network.TestNetworkPassphrase},
			b.SourceAccount{AddressOrSeed: hostAcct.Address()},
			b.AccountMerge(
				b.SourceAccount{AddressOrSeed: hostAcct.Address()},
				b.Destination{AddressOrSeed: dest},
			))
		k := key.DeriveAccountPrimary(g.seed)
		env, err := closeAccountBuilder.Sign(k.Seed())
		g.addTxTask(root.Tx(), walletBucket, *env.E)
		return nil
	})
}

// nextChannelKeyIndex reads the next unused key path index from bu
// and returns after bumping the stored key path index.
func nextChannelKeyIndex(agent *db.Agent, bump uint32) uint32 {
	i := agent.NextKeypathIndex()
	agent.PutNextKeypathIndex(i + bump)
	return i
}

// DoCommand executes c on channel channelID.
func (g *Agent) DoCommand(channelID string, c *fsm.Command) error {
	if len(channelID) == 0 {
		return errNoChannelSpecified
	}
	if c.Name == "" {
		return errNoCommandSpecified
	}
	return g.updateChannel(channelID, func(root *db.Root, updater *fsm.Updater, update *Update) error {
		if !root.Agent().Ready() {
			return errAgentClosing
		}
		update.InputCommand = c
		return updater.Cmd(c)
	})
}

func (g *Agent) scheduleTimer(tx *bolt.Tx, t time.Time, chanID string) {
	tx.OnCommit(func() {
		// TODO(bobg): this should be cancelable.
		g.wclient.AfterFunc(t, func() {
			err := g.updateChannel(chanID, func(_ *db.Root, updater *fsm.Updater, update *Update) error {
				update.InputLedgerTime = g.wclient.Now()
				return updater.Time()
			})
			if err != nil {
				g.debugf("scheduling timer on channel %s: %s", string(chanID), err)
				g.mustDeauthenticate()
			}
		})
	})
}

func (g *Agent) passphrase(root *db.Root) string {
	return network.TestNetworkPassphrase
}

// PeerHandler handles RPCs
// (such as ProposeChannel, AcceptChannel, Payment, etc.)
// from remote channel endpoints.
func (g *Agent) PeerHandler() http.Handler {
	g.once.Do(func() {
		mux := new(http.ServeMux)
		mux.HandleFunc("/starlight/message", g.handleMsg)
		mux.HandleFunc("/federation", g.handleFed)
		mux.HandleFunc("/.well-known/stellar.toml", g.handleTOML)
		g.handler = mux
	})
	return g.handler
}

func (g *Agent) handleMsg(w http.ResponseWriter, req *http.Request) {
	m := new(fsm.Message)
	err := json.NewDecoder(req.Body).Decode(m)
	if err != nil {
		WriteError(req, w, errors.Sub(ErrUnmarshaling, err))
		return
	}
	if len(m.ChannelID) == 0 {
		WriteError(req, w, errors.Sub(errNoChannelSpecified, err))
		return
	}
	var (
		guestSeqNum, hostSeqNum, baseSeqNum xdr.SequenceNumber
		escrowAcct                          xdr.AccountId
		hostAccount                         string
	)
	if m.ChannelProposeMsg != nil {
		propose := m.ChannelProposeMsg
		chanID, err := g.checkChannelUnique(propose.HostAcct.Address(), propose.GuestAcct.Address())
		if err != nil {
			err = g.resolveChannelCreateConflict(chanID, propose)
			if err != nil {
				WriteError(req, w, err)
			}
			return
		}
		err = escrowAcct.SetAddress(string(m.ChannelID))
		if err != nil {
			WriteError(req, w, errors.Sub(errInvalidChannelID, err))
			return
		}
		baseSeqNum, guestSeqNum, hostSeqNum, err = g.getSequenceNumbers(m.ChannelID, propose.GuestRatchetAcct, propose.HostRatchetAcct)
		if err != nil {
			WriteError(req, w, errors.Sub(errFetchingAccounts, err))
			return
		}
		hostAccount, _, err = g.FindAccount(propose.HostAcct.Address())
		if err != nil {
			hostAccount = ""
		}
	}
	// Drop received RPC messages if agent is the Host. Only Hosts should send messages through RPC, the
	// Guest's messages are retrieved through the Host sending long-polling HTTP requests to /api/messages
	if g.channelRole(m.ChannelID) == fsm.Host {
		WriteError(req, w, errRemoteGuestMessage)
		return
	}
	err = g.updateChannel(m.ChannelID, func(root *db.Root, updater *fsm.Updater, update *Update) error {
		if m.ChannelProposeMsg != nil {
			maxRoundDur := time.Minute * time.Duration(root.Agent().Config().MaxRoundDurMins())
			finalityDelay := time.Minute * time.Duration(root.Agent().Config().FinalityDelayMins())
			if m.ChannelProposeMsg.MaxRoundDuration != maxRoundDur {
				return errors.Wrapf(errBadRequest, "channel proposed with max round dur %s, want %s", m.ChannelProposeMsg.MaxRoundDuration, maxRoundDur)
			}
			if m.ChannelProposeMsg.FinalityDelay != finalityDelay {
				return errors.Wrapf(errBadRequest, "channel proposed with finality delay %s, want %s", m.ChannelProposeMsg.FinalityDelay, finalityDelay)
			}
			if hostAccount != "" {
				updater.C.CounterpartyAddress = hostAccount
			} else {
				updater.C.CounterpartyAddress = m.ChannelProposeMsg.HostAcct.Address()
			}
			updater.C.Role = fsm.Guest
			updater.C.EscrowAcct = fsm.AccountID(escrowAcct)
			updater.C.HostAcct = m.ChannelProposeMsg.HostAcct
			updater.C.GuestAcct = *root.Agent().PrimaryAcct()
			updater.C.GuestRatchetAcctSeqNum = guestSeqNum
			updater.C.HostRatchetAcctSeqNum = hostSeqNum
			updater.C.BaseSequenceNumber = baseSeqNum
		}
		update.InputMessage = m
		return updater.Msg(m)
	})
	if err != nil {
		g.debugf("handling RPC message, channel %s: %s", string(m.ChannelID), err)
		WriteError(req, w, err)
	}
	return
}

func (g *Agent) resolveChannelCreateConflict(chanID []byte, propose *fsm.ChannelProposeMsg) error {
	return db.Update(g.db, func(root *db.Root) error {
		if !root.Agent().Ready() {
			return errAgentClosing
		}

		var (
			proposedHostAddr  = propose.HostAcct.Address()
			proposedGuestAddr = propose.GuestAcct.Address()
		)

		chans := root.Agent().Channels()
		c := chans.Get(chanID)
		switch c.State {
		case fsm.SettingUp:
			return errors.Wrapf(errChannelExistsRetriable, "setting up: host %s, guest %s", proposedHostAddr, proposedGuestAddr)

		case fsm.ChannelProposed:
			if propose.HostAmount < c.HostAmount || (propose.HostAmount == c.HostAmount && proposedHostAddr < c.HostAcct.Address()) {
				g.logf("channel proposal from %s takes precedence", proposedHostAddr)
				return errors.Wrapf(errExists, "channel proposed: host %s, guest %s", proposedHostAddr, proposedGuestAddr)
			}
			g.logf("my channel proposal takes precedence over the one from %s", proposedHostAddr)
			go g.DoCommand(string(chanID), &fsm.Command{
				Name: "CleanUp",
			})
			return errors.Wrapf(errChannelExistsRetriable, "channel proposed: host %s, guest %s", proposedHostAddr, proposedGuestAddr)

		case fsm.AwaitingCleanup:
			// Channel in cleanup process: counterparty should retry until cleanup is complete
			return errors.Wrapf(errChannelExistsRetriable, "awaiting cleanup: host %s, guest %s", proposedHostAddr, proposedGuestAddr)

		default:
			// Channel is already open or in some payment state: reject the proposed channel
			return errors.Wrapf(errExists, "host %s, guest %s", proposedHostAddr, proposedGuestAddr)
		}
	})
}

func (g *Agent) channelRole(chanID string) (role fsm.Role) {
	db.View(g.db, func(root *db.Root) error {
		chans := root.Agent().Channels()
		c := chans.Get([]byte(chanID))
		role = c.Role
		return nil
	})
	return role
}

func (g *Agent) handleFed(w http.ResponseWriter, req *http.Request) {
	if req.URL.Query().Get("type") != "name" {
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}

	var name, acct string
	db.View(g.db, func(root *db.Root) error {
		name = root.Agent().Config().Username()
		acct = root.Agent().PrimaryAcct().Address()
		return nil
	})

	q := req.URL.Query().Get("q")
	if q != name+"*"+req.Host {
		http.Error(w, "not found", 404)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{
		"stellar_address": q + "*" + req.Host,
		"account_id":      acct,
	})
}

func (g *Agent) handleTOML(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "text/plain")
	v := struct{ Origin string }{protocol(req.Host) + req.Host}
	tomlTemplate.Execute(w, v)
}

func (g *Agent) getSequenceNumbers(chanID string, guestRatchetAcct, hostRatchetAcct fsm.AccountID) (base, guest, host xdr.SequenceNumber, err error) {
	var escrowAcct xdr.AccountId
	err = escrowAcct.SetAddress(chanID)
	if err != nil {
		return 0, 0, 0, err
	}
	base, err = g.wclient.SequenceForAccount(escrowAcct.Address())
	if err != nil {
		return 0, 0, 0, err
	}
	guest, err = g.wclient.SequenceForAccount(guestRatchetAcct.Address())
	if err != nil {
		return 0, 0, 0, err
	}
	host, err = g.wclient.SequenceForAccount(hostRatchetAcct.Address())
	if err != nil {
		return 0, 0, 0, err
	}
	return base, guest, host, nil
}

// sleep sleeps until dur elapses or g's context is canceled.
func (g *Agent) sleep(dur time.Duration) {
	t := time.NewTimer(dur)
	select {
	case <-t.C:
	case <-g.rootCtx.Done():
		t.Stop()
	}
}

func (g *Agent) LoadAccount(accountID string) (worizon.Account, error) {
	return g.wclient.LoadAccount(accountID)
}

func (g *Agent) AfterFunc(t time.Time, f func()) {
	g.wclient.AfterFunc(t, f)
}

func (g *Agent) Now() time.Time {
	return g.wclient.Now()
}

var tomlTemplate = template.Must(template.New("toml").Parse(`
FEDERATION_SERVER="{{.Origin}}/federation"
STARLIGHT_SERVER="{{.Origin}}/"`))
