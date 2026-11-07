// Package tesla provides Tesla vehicle charging control via BLE.
//
// Uses the tesla/vehicle-command Go library directly (no CLI shelling).
// Commands are sent unconditionally (no internal state tracking) because the
// car's state can change externally (Tesla app, scheduled charging, charge
// complete, etc.). The car is the source of truth, not this system.
//
// The only optimization is skipping SetChargingAmps when the value hasn't
// changed, to reduce BLE chatter.
package tesla

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/teslamotors/vehicle-command/pkg/cache"
	"github.com/teslamotors/vehicle-command/pkg/connector/ble"
	"github.com/teslamotors/vehicle-command/pkg/protocol"
	"github.com/teslamotors/vehicle-command/pkg/vehicle"

	carserver "github.com/teslamotors/vehicle-command/pkg/protocol/protobuf/carserver"
	pbkeys "github.com/teslamotors/vehicle-command/pkg/protocol/protobuf/keys"
	universal "github.com/teslamotors/vehicle-command/pkg/protocol/protobuf/universalmessage"
	"github.com/teslamotors/vehicle-command/pkg/protocol/protobuf/vcsec"
)

// ErrAlreadyCharging is returned by sendCommand when the car reports it is
// already charging. This allows callers to distinguish "started fresh" from
// "was already charging".
var ErrAlreadyCharging = errors.New("car is already charging")

// ErrChargeComplete is returned by StartCharging when the car refuses to start
// because charging is complete: the battery is at (or above) its charge limit.
// The command reached the car and the link is fine; the car simply has no room.
var ErrChargeComplete = errors.New("car is at its charge limit")

// isChargeCompleteRefusal reports whether err is the car's "complete" answer to
// ChargeStart. vehicle-command reports it as a NominalError whose text is the
// car's plain-text reason: "car could not execute command: complete".
func isChargeCompleteRefusal(err error) bool {
	return err != nil && strings.HasSuffix(strings.TrimSpace(err.Error()), "could not execute command: complete")
}

// ErrPreempted is returned by background BLE work (SoC reads, wake-up) that was
// aborted so a charging-control command could use the radio instead.
var ErrPreempted = errors.New("preempted by a control command")

const (
	// bleConnectTimeout is how long background work (SoC reads, wake-up) waits
	// for BLE scan + connection to the car.
	bleConnectTimeout = 20 * time.Second

	// bleCommandTimeout is how long background work waits for a command response.
	bleCommandTimeout = 10 * time.Second

	// bleControlConnectTimeout / bleControlCommandTimeout bound a charging-control
	// command (set amps, start, stop). They are deliberately tight: a command that
	// can't reach the car in ~15 s is abandoned and retried on the next cycle,
	// rather than holding the radio for the better part of a minute.
	bleControlConnectTimeout = 10 * time.Second
	bleControlCommandTimeout = 5 * time.Second

	// bleMaxSlotRetries is the number of extra connect attempts we make when
	// the car reports "maximum number of BLE devices" — it usually clears
	// stale peerings within a few seconds.
	bleMaxSlotRetries = 3

	// bleSlotRetryDelay is the base delay between retries; grows with attempt number.
	bleSlotRetryDelay = 3 * time.Second

	// bleSlotFullCooldown is how long to suppress further BLE attempts after
	// a slot-full failure exhausts the in-call retries. Gives the car time to
	// age out whatever peers are holding its limit and stops us from filling
	// its connection table with our own failing probes every 5s.
	bleSlotFullCooldown = 60 * time.Second
)

// isCarSlotFullError returns true if the error is the car's "peer table full"
// rejection. Occurs when another BLE peer (phone, Tesla app, watch) holds a slot
// or when a previous teardown hasn't yet aged out on the car's side.
func isCarSlotFullError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "maximum number of BLE devices")
}

// BLEClient communicates with a Tesla vehicle over Bluetooth Low Energy
// using the tesla/vehicle-command Go library.
// In dry-run mode, it logs what it would do without sending actual commands.
type BLEClient struct {
	dryRun     bool
	vin        string
	keyFile    string // path to the PEM private key (used when rotating keys)
	privateKey protocol.ECDHPrivateKey
	sessCache  *cache.SessionCache
	lastAmps   int // last amps value sent, to skip redundant BLE calls
	store      *Store

	// dryRunState, if set, supplies the dry-run car reading instead of the
	// fixed one (the demo simulates a car whose battery fills up).
	dryRunState func() *VehicleChargeState

	mu            sync.Mutex // serializes BLE commands (only one connection at a time)
	slotFullUntil time.Time  // suppress BLE attempts until this time (after slot-full)
	pairing       bool       // when true, normal commands return a "paused for pairing" error

	// Low-priority work registers a cancel func here so a control command can
	// abort it instead of queueing behind it on mu. Guarded by bgMu, not mu,
	// because the whole point is to reach them while mu is held.
	bgMu       sync.Mutex
	socCancel  context.CancelFunc // in-flight (or waiting) SoC read
	wakeCancel context.CancelFunc // in-flight (or waiting) wake-up
}

// bleBudget is the time allowed for the connect+handshake phase and for the
// command itself, each counted from when the radio lock is acquired.
type bleBudget struct {
	connect time.Duration
	command time.Duration
}

var (
	// controlBudget applies to commands that change what the car is doing.
	controlBudget = bleBudget{connect: bleControlConnectTimeout, command: bleControlCommandTimeout}

	// backgroundBudget applies to preemptible work, which can afford to be patient.
	backgroundBudget = bleBudget{
		connect: bleConnectTimeout + bleMaxSlotRetries*bleSlotRetryDelay*2,
		command: bleCommandTimeout,
	}
)

// bleCall describes how sendCommand should run one command.
type bleCall struct {
	budget bleBudget
	// urgent commands bypass the slot-full cooldown: that cooldown exists to stop
	// background probing from filling the car's peer table, but a command that
	// reduces or stops the charge must still be attempted.
	urgent bool
}

// PreemptBackground aborts low-priority BLE work so a control command isn't
// queued behind it. SoC reads are always aborted; the wake-up only when urgent,
// since a normal start command usually wants the wake-up to finish first.
func (b *BLEClient) PreemptBackground(urgent bool) {
	b.bgMu.Lock()
	soc, wake := b.socCancel, b.wakeCancel
	b.bgMu.Unlock()
	if soc != nil {
		soc()
	}
	if urgent && wake != nil {
		wake()
	}
}

// NewBLEClient creates a BLE client for the given VIN and private key.
// If dryRun is true, commands are logged but not sent.
// keyFile is the PEM path, used only when the caller invokes Pair(rotate=true).
func NewBLEClient(dryRun bool, vin string, keyFile string, privateKey protocol.ECDHPrivateKey, store *Store) *BLEClient {
	return &BLEClient{
		dryRun:     dryRun,
		vin:        vin,
		keyFile:    keyFile,
		privateKey: privateKey,
		sessCache:  cache.New(0), // in-memory session cache, no disk persistence
		lastAmps:   -1,
		store:      store,
	}
}

// dialBLE opens a BLE connection to the car, retrying with backoff if the
// car's peer table is currently full.
func (b *BLEClient) dialBLE(ctx context.Context, op string) (*ble.Connection, error) {
	var lastErr error
	for attempt := 0; attempt <= bleMaxSlotRetries; attempt++ {
		conn, err := ble.NewConnection(ctx, b.vin)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if !isCarSlotFullError(err) || attempt == bleMaxSlotRetries {
			return nil, err
		}
		delay := bleSlotRetryDelay * time.Duration(attempt+1)
		log.Printf("[TESLA] BLE slot full for %s — car ageing stale peers, retry in %s (attempt %d/%d)",
			op, delay, attempt+1, bleMaxSlotRetries)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

// sendCommand connects to the car via BLE, starts an authenticated session,
// executes the command function, and disconnects. The session cache reduces
// handshake overhead on subsequent commands.
//
// parent bounds the whole call: cancelling it aborts the connect, handshake or
// command in progress, which is how background work is preempted. The call's
// own time budget starts once the radio lock is held, so time spent waiting
// behind another command doesn't eat into it.
func (b *BLEClient) sendCommand(parent context.Context, name string, call bleCall, fn func(ctx context.Context, v *vehicle.Vehicle) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.pairing {
		return fmt.Errorf("BLE connect for %s: paused — pairing in progress", name)
	}
	if remaining := time.Until(b.slotFullUntil); remaining > 0 && !call.urgent {
		return fmt.Errorf("BLE connect for %s: slot-full cooldown — %s remaining",
			name, remaining.Round(time.Second))
	}

	connectCtx, connectCancel := context.WithTimeout(parent, call.budget.connect)
	defer connectCancel()

	// Scan for the car and establish BLE connection (with slot-full retry)
	conn, err := b.dialBLE(connectCtx, name)
	if err != nil {
		if isCarSlotFullError(err) {
			b.slotFullUntil = time.Now().Add(bleSlotFullCooldown)
			log.Printf("[TESLA] BLE slot-full; pausing BLE attempts for %s", bleSlotFullCooldown)
		}
		return fmt.Errorf("BLE connect for %s: %w", name, err)
	}
	defer conn.Close()

	// Create vehicle instance with session caching
	v, err := vehicle.NewVehicle(conn, b.privateKey, b.sessCache)
	if err != nil {
		return fmt.Errorf("create vehicle for %s: %w", name, err)
	}

	// Connect the protocol layer (handshake)
	if err := v.Connect(connectCtx); err != nil {
		return fmt.Errorf("vehicle connect for %s: %w", name, err)
	}

	// Start sessions for both VCSEC and Infotainment domains.
	// Charging commands (SetChargingAmps, ChargeStart, ChargeStop) require Infotainment.
	if err := v.StartSession(connectCtx, []universal.Domain{
		universal.Domain_DOMAIN_VEHICLE_SECURITY,
		universal.Domain_DOMAIN_INFOTAINMENT,
	}); err != nil {
		return fmt.Errorf("start session for %s: %w", name, err)
	}

	// Execute the actual command with its own timeout
	cmdCtx, cmdCancel := context.WithTimeout(parent, call.budget.command)
	defer cmdCancel()

	if err := fn(cmdCtx, v); err != nil {
		if parent.Err() != nil {
			// Aborted on purpose — not a command failure worth logging.
			return parent.Err()
		}
		errMsg := err.Error()
		// Treat idempotent responses as success (car already in requested state)
		if strings.Contains(errMsg, "is_charging") {
			log.Printf("[TESLA] %s: car is already charging (OK)", name)
			if b.store != nil {
				b.store.LogBLECommand(name, nil)
			}
			return ErrAlreadyCharging
		}
		if strings.Contains(errMsg, "not_charging") {
			log.Printf("[TESLA] %s: car is already stopped (OK)", name)
			if b.store != nil {
				b.store.LogBLECommand(name, nil)
			}
			return nil
		}
		if isChargeCompleteRefusal(err) {
			// The car answered: charging is complete. Not a link failure.
			log.Printf("[TESLA] %s: car is at its charge limit (OK)", name)
			if b.store != nil {
				b.store.LogBLECommand(name, nil)
			}
			return ErrChargeComplete
		}
		cmdErr := fmt.Errorf("%s: %w", name, err)
		if b.store != nil {
			b.store.LogBLECommand(name, cmdErr)
		}
		return cmdErr
	}

	log.Printf("[TESLA] %s OK", name)
	if b.store != nil {
		b.store.LogBLECommand(name, nil)
	}
	return nil
}

// SetChargingAmps sets the charging current.
// Skips the BLE call if the value hasn't changed since last sent. urgent marks a
// safety reduction, which is attempted even during the slot-full cooldown.
func (b *BLEClient) SetChargingAmps(amps int, urgent bool) error {
	if amps == b.lastAmps {
		return nil
	}
	b.lastAmps = amps

	if b.dryRun {
		log.Printf("[TESLA DRY RUN] Set charging amps to %d", amps)
		return nil
	}

	err := b.sendCommand(context.Background(), fmt.Sprintf("SetChargingAmps(%d)", amps),
		bleCall{budget: controlBudget, urgent: urgent},
		func(ctx context.Context, v *vehicle.Vehicle) error {
			return v.SetChargingAmps(ctx, int32(amps))
		})
	if err != nil {
		b.lastAmps = -1 // reset so next call retries
		return err
	}
	return nil
}

// StartCharging tells the car to begin charging.
// Returns (true, nil) if the car was already charging (someone else started it).
// Returns (false, nil) if we successfully started a fresh charge session.
// Returns ErrChargeComplete if the car is at its charge limit and won't start.
func (b *BLEClient) StartCharging() (bool, error) {
	if b.dryRun {
		// A simulated car (the demo) refuses like a real one once it is full.
		if b.dryRunState != nil {
			if cs := b.dryRunState(); cs != nil && cs.BatteryLevel >= cs.ChargeLimitSoc {
				log.Printf("[TESLA DRY RUN] Start charging: car is at its charge limit")
				return false, ErrChargeComplete
			}
		}
		log.Printf("[TESLA DRY RUN] Start charging")
		return false, nil
	}

	err := b.sendCommand(context.Background(), "ChargeStart", bleCall{budget: controlBudget},
		func(ctx context.Context, v *vehicle.Vehicle) error {
			return v.ChargeStart(ctx)
		})
	if errors.Is(err, ErrAlreadyCharging) {
		return true, nil
	}
	return false, err
}

// WakeUp sends a BLE wake signal to the car's VCSEC domain.
// VCSEC remains accessible even when the car is in deep sleep, unlike Infotainment.
// After this call, the car typically becomes fully responsive within 10–30 seconds.
//
// Runs as preemptible background work: an urgent control command (see
// PreemptBackground) aborts it and WakeUp returns ErrPreempted.
func (b *BLEClient) WakeUp() error {
	if b.dryRun {
		log.Printf("[TESLA DRY RUN] WakeUp")
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.bgMu.Lock()
	b.wakeCancel = cancel
	b.bgMu.Unlock()

	err := b.wakeUp(ctx)
	if err != nil && ctx.Err() != nil {
		return ErrPreempted
	}
	return err
}

func (b *BLEClient) wakeUp(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.pairing {
		return fmt.Errorf("BLE connect for WakeUp: paused — pairing in progress")
	}
	if remaining := time.Until(b.slotFullUntil); remaining > 0 {
		return fmt.Errorf("BLE connect for WakeUp: slot-full cooldown — %s remaining",
			remaining.Round(time.Second))
	}

	connectCtx, connectCancel := context.WithTimeout(ctx, backgroundBudget.connect)
	defer connectCancel()

	conn, err := b.dialBLE(connectCtx, "WakeUp")
	if err != nil {
		if isCarSlotFullError(err) {
			b.slotFullUntil = time.Now().Add(bleSlotFullCooldown)
			log.Printf("[TESLA] BLE slot-full; pausing BLE attempts for %s", bleSlotFullCooldown)
		}
		return fmt.Errorf("BLE connect for WakeUp: %w", err)
	}
	defer conn.Close()

	v, err := vehicle.NewVehicle(conn, b.privateKey, b.sessCache)
	if err != nil {
		return fmt.Errorf("create vehicle for WakeUp: %w", err)
	}

	if err := v.Connect(connectCtx); err != nil {
		return fmt.Errorf("vehicle connect for WakeUp: %w", err)
	}

	// Start VCSEC session only — Infotainment may be offline when the car is sleeping.
	// The wake RKE action is handled entirely by VCSEC.
	if err := v.StartSession(connectCtx, []universal.Domain{
		universal.Domain_DOMAIN_VEHICLE_SECURITY,
	}); err != nil {
		return fmt.Errorf("VCSEC session for WakeUp: %w", err)
	}

	cmdCtx, cmdCancel := context.WithTimeout(ctx, backgroundBudget.command)
	defer cmdCancel()

	if err := v.Wakeup(cmdCtx); err != nil {
		if b.store != nil {
			b.store.LogBLECommand("WakeUp", err)
		}
		return fmt.Errorf("WakeUp command: %w", err)
	}

	log.Printf("[TESLA] WakeUp OK — car should be ready in ~30s")
	if b.store != nil {
		b.store.LogBLECommand("WakeUp", nil)
	}
	return nil
}

// StopCharging tells the car to stop charging.
// Sent unconditionally — idempotent if the car is already stopped.
func (b *BLEClient) StopCharging() error {
	b.lastAmps = -1 // reset so next start re-sends amps

	if b.dryRun {
		log.Printf("[TESLA DRY RUN] Stop charging")
		return nil
	}

	// Stopping is always urgent: it's the last line of defence against overload.
	return b.sendCommand(context.Background(), "ChargeStop", bleCall{budget: controlBudget, urgent: true},
		func(ctx context.Context, v *vehicle.Vehicle) error {
			return v.ChargeStop(ctx)
		})
}

// VehicleChargeState is a snapshot of the car's charge data read over BLE via
// GetState(charge). All percentages are 0–100; energy is kWh; range is km.
type VehicleChargeState struct {
	BatteryLevel       int     `json:"battery_level"`           // SoC % (what the car's UI shows)
	UsableBatteryLevel int     `json:"usable_battery_level"`    // usable SoC % (drops when cold)
	ChargeLimitSoc     int     `json:"charge_limit_soc"`        // target charge limit %
	ChargingState      string  `json:"charging_state"`          // Charging|Stopped|Complete|Starting|Disconnected|NoPower|Calibrating
	MinutesToFull      int     `json:"minutes_to_full"`         // est. minutes to reach the charge limit
	ChargeEnergyAdded  float64 `json:"charge_energy_added_kwh"` // energy added this session, kWh
	BatteryRangeKm     float64 `json:"battery_range_km"`        // rated range, km
	ChargeCurrentA     int     `json:"charge_current_a"`        // the car's own charging-current setting, A
}

// ReadChargeState queries the car for its charge state (SoC, charge limit,
// charging status) over BLE. Requires the Infotainment domain — the same
// session the charging commands use — so the car must be awake. When the car
// is asleep this returns the usual "listener timed out" style error.
//
// In dry-run mode it returns plausible synthetic data so the UI can be
// exercised without a real car.
//
// Runs as preemptible background work: a control command (see
// PreemptBackground) aborts it and ReadChargeState returns ErrPreempted.
func (b *BLEClient) ReadChargeState() (*VehicleChargeState, error) {
	if b.dryRun {
		if b.dryRunState != nil {
			return b.dryRunState(), nil
		}
		return &VehicleChargeState{
			BatteryLevel:       64,
			UsableBatteryLevel: 63,
			ChargeLimitSoc:     80,
			ChargingState:      "Charging",
			MinutesToFull:      95,
			ChargeEnergyAdded:  7.4,
			BatteryRangeKm:     289,
			ChargeCurrentA:     16,
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.bgMu.Lock()
	b.socCancel = cancel
	b.bgMu.Unlock()

	var out *VehicleChargeState
	err := b.sendCommand(ctx, "GetState(charge)", bleCall{budget: backgroundBudget},
		func(ctx context.Context, v *vehicle.Vehicle) error {
			data, err := v.GetState(ctx, vehicle.StateCategoryCharge)
			if err != nil {
				return err
			}
			cs := data.GetChargeState()
			if cs == nil {
				return fmt.Errorf("no charge state in response")
			}
			out = &VehicleChargeState{
				BatteryLevel:       int(cs.GetBatteryLevel()),
				UsableBatteryLevel: int(cs.GetUsableBatteryLevel()),
				ChargeLimitSoc:     int(cs.GetChargeLimitSoc()),
				ChargingState:      chargingStateString(cs.GetChargingState()),
				MinutesToFull:      int(cs.GetMinutesToFullCharge()),
				ChargeEnergyAdded:  float64(cs.GetChargeEnergyAdded()),
				BatteryRangeKm:     float64(cs.GetBatteryRange()) * 1.609344, // proto reports miles
				ChargeCurrentA:     int(cs.GetChargeCurrentRequest()),
			}
			return nil
		})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ErrPreempted
		}
		return nil, err
	}
	return out, nil
}

// chargingStateString maps the protobuf charging-state oneof to a stable label.
func chargingStateString(cs *carserver.ChargeState_ChargingState) string {
	switch {
	case cs == nil:
		return ""
	case cs.GetCharging() != nil:
		return "Charging"
	case cs.GetComplete() != nil:
		return "Complete"
	case cs.GetStopped() != nil:
		return "Stopped"
	case cs.GetStarting() != nil:
		return "Starting"
	case cs.GetNoPower() != nil:
		return "NoPower"
	case cs.GetDisconnected() != nil:
		return "Disconnected"
	case cs.GetCalibrating() != nil:
		return "Calibrating"
	default:
		return ""
	}
}

// Pair runs the add-key flow over BLE.
//
// If rotate is true, the existing key file (if any) is backed up with a
// timestamp suffix and a fresh P-256 key pair is generated. Rotation is
// required when the previous key was deleted from the car's Phone Keys list
// — Tesla firmware rejects re-enrollment of a previously-removed public key.
//
// onProgress is called with human-readable status messages as the flow
// advances so callers (e.g. an HTTP SSE endpoint) can stream progress.
//
// While Pair is active, other BLE commands on this client return a
// "paused — pairing in progress" error without touching the radio.
//
// Returns nil when the car confirms the new key; otherwise an error.
func (b *BLEClient) Pair(ctx context.Context, rotate bool, windowS int, onProgress func(string)) error {
	if windowS < 60 {
		windowS = 60
	}
	if onProgress == nil {
		onProgress = func(string) {}
	}

	b.mu.Lock()
	if b.pairing {
		b.mu.Unlock()
		return fmt.Errorf("pairing already in progress")
	}
	b.pairing = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.pairing = false
		b.mu.Unlock()
	}()

	if rotate {
		onProgress("Rotating key pair (backing up existing)…")
		if err := rotateKeyFiles(b.keyFile); err != nil {
			return fmt.Errorf("rotate key: %w", err)
		}
	}

	priv, pub, err := loadOrGenerateKey(b.keyFile)
	if err != nil {
		return fmt.Errorf("key: %w", err)
	}
	// Swap in the (possibly new) private key for future normal commands.
	b.privateKey = priv
	b.sessCache = cache.New(0)
	b.lastAmps = -1

	onProgress("Scanning for vehicle over BLE (up to 30 s)…")
	connectCtx, cancelConnect := context.WithTimeout(ctx, 30*time.Second)
	defer cancelConnect()

	conn, err := ble.NewConnection(connectCtx, b.vin)
	if err != nil {
		return fmt.Errorf("BLE connect: %w", err)
	}
	defer conn.Close()

	v, err := vehicle.NewVehicle(conn, priv, nil)
	if err != nil {
		return fmt.Errorf("new vehicle: %w", err)
	}
	if err := v.Connect(connectCtx); err != nil {
		return fmt.Errorf("vehicle connect: %w", err)
	}
	defer v.Disconnect()

	onProgress("Sending add-key request…")
	reqCtx, cancelReq := context.WithTimeout(ctx, 10*time.Second)
	defer cancelReq()
	if err := v.SendAddKeyRequestWithRole(reqCtx, pub, pbkeys.Role_ROLE_OWNER, vcsec.KeyFormFactor_KEY_FORM_FACTOR_CLOUD_KEY); err != nil {
		return fmt.Errorf("send add-key request: %w", err)
	}

	onProgress("Now tap your NFC keycard on the center console tray, then press Confirm on the vehicle screen.")

	deadline := time.Now().Add(time.Duration(windowS) * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timed out after %d s — no confirmation from the car", windowS)
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		pollCtx, pollCancel := context.WithTimeout(ctx, 5*time.Second)
		info, perr := v.SessionInfo(pollCtx, pub, universal.Domain_DOMAIN_INFOTAINMENT)
		pollCancel()
		if perr == nil && info != nil {
			onProgress("Key enrolled. Pairing complete.")
			return nil
		}

		onProgress(fmt.Sprintf("Waiting for keycard tap on center console… %s remaining.", remaining.Round(time.Second)))
		time.Sleep(3 * time.Second)
	}
}

// rotateKeyFiles backs up tesla-key.pem and tesla-key.pem.pub with a
// timestamp suffix so the next load generates a fresh pair.
func rotateKeyFiles(keyFile string) error {
	ts := time.Now().UTC().Format("20060102-150405")
	for _, suffix := range []string{"", ".pub"} {
		src := keyFile + suffix
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := src + ".bak-" + ts
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("backup %s: %w", src, err)
		}
		log.Printf("[TESLA] Backed up %s -> %s", src, dst)
	}
	return nil
}

// loadOrGenerateKey loads the existing key pair, or generates a fresh one if
// the file is missing. Used by the pairing flow.
func loadOrGenerateKey(keyFile string) (protocol.ECDHPrivateKey, *ecdh.PublicKey, error) {
	if _, err := os.Stat(keyFile); err != nil {
		ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		kb, err := x509.MarshalECPrivateKey(ecdsaKey)
		if err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0600); err != nil {
			return nil, nil, err
		}
		pb, err := x509.MarshalPKIXPublicKey(&ecdsaKey.PublicKey)
		if err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(keyFile+".pub", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pb}), 0644); err != nil {
			return nil, nil, err
		}
		log.Printf("[TESLA] Generated fresh key pair at %s", keyFile)
	}
	priv, err := protocol.LoadPrivateKey(keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("load private: %w", err)
	}
	pub, err := protocol.LoadPublicKey(keyFile + ".pub")
	if err != nil {
		return nil, nil, fmt.Errorf("load public: %w", err)
	}
	return priv, pub, nil
}
