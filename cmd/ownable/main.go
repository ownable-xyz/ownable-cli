// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Command ownable renders the tokens of a KVM piece straight from Ethereum,
// through the piece's own on-chain render path, using nothing but eth_call.
//
//	ownable info   <piece>
//	ownable render <piece> <tokenId> [-o out.png]
//	ownable verify <piece> <tokenId> [--against file.png | URL | ipfs://CID/path]
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/ownable-xyz/ownable-cli/internal/abi"
	"github.com/ownable-xyz/ownable-cli/internal/fork"
	"github.com/ownable-xyz/ownable-cli/internal/history"
	"github.com/ownable-xyz/ownable-cli/internal/imgout"
	"github.com/ownable-xyz/ownable-cli/internal/piece"
	"github.com/ownable-xyz/ownable-cli/internal/ref"
	"github.com/ownable-xyz/ownable-cli/internal/rpc"
	"github.com/ownable-xyz/ownable-cli/internal/stamp"
	"github.com/ownable-xyz/ownable-cli/internal/stepkvm"
	"github.com/ownable-xyz/ownable-cli/internal/ui"
)

// tagline sits beside the wordmark in the header.
const tagline = "eth_call"

var version = "v0.2.0"

const usage = `ownable: render KVM pieces from Ethereum, trustlessly, through their own contracts

usage:
  ownable info   <piece> [flags]
  ownable render <piece> <tokenId> [-o out.png] [flags]
  ownable verify <piece> <tokenId> [--against REF] [flags]
  ownable version

<piece> is the address of a contract that implements IKohiPiece.

common flags:
  --rpc URL          JSON-RPC endpoint (default: $OWNABLE_RPC_URL, then $ETH_RPC_URL)

render and verify flags:
  -o, --out FILE     output PNG (default <symbol>-<tokenId>.png)
  --gas-cap N        gas limit of every eth_call (default 50000000)
  --band N           raster band height in pixels (default 32)
  --workers N        raster bands rendered at the same time (default 8)
  --meter MODE       how to measure gas: auto, trace, estimate, off (default auto)
  --measure-every N  measure one call in N, after the first 8 (default 10;
                     1 measures every call for an exact gas total, at the
                     cost of one extra request per call)
  --save-stream FILE also write the draw stream the chain produced
  --stream FILE      skip the interpreter and rasterize this saved draw stream
                     (the pixels are still computed on chain, but the stream
                     is then only as trustworthy as the file)
  --checkpoint FILE  save the interpreter's progress to FILE and resume from it
                     (default <out>.ckpt; removed when the render is complete)
  --no-checkpoint    do not save or resume progress
  --noise-lib ADDR   NoiseV1Ext address, if the renderer does not expose one
  --quiet            print only the result lines
  --no-color         no color on a terminal (also: the NO_COLOR variable)

verify flags:
  --against REF      the image to compare with: a local PNG, an https:// URL,
                     ipfs://CID/path, or a bare CID or CID/path. Without it,
                     verify reads the token's tokenURI and uses its "image".
  --ipfs-gateway URL IPFS gateway prefix, e.g. https://ipfs.filebase.io/ipfs/
                     (repeatable; tried in order; default a short public list)
  --fetch-timeout D  timeout of each download attempt (default 60s)
  --no-cache         download again instead of using the local cache

fork mode (faster, weaker):
  --fork             start a local anvil forked from --rpc at its latest block
                     and render against it; anvil is killed afterwards
  --fork-block N     fork at block N instead of the latest
  --fork-url URL     render against an already-running local node instead
  --anvil PATH       the anvil executable (default: anvil on PATH)
  --chunk-steps N    interpreter steps per call in fork mode (default
                     2000000)

Every call is an eth_call (a read). Nothing is ever signed or sent.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "info":
		err = cmdInfo(ctx, os.Args[2:])
	case "render":
		err = cmdRender(ctx, os.Args[2:], false)
	case "verify":
		err = cmdRender(ctx, os.Args[2:], true)
	case "version", "--version", "-v":
		fmt.Println("ownable", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "ownable: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, errMismatch) {
			os.Exit(1) // the report already says how
		}
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			err = errors.New("interrupted")
		}
		ui.New(os.Stderr, false, hasArg("--no-color")).Fail(err, "")
		os.Exit(1)
	}
}

// hasArg reports whether a flag was given, before the flags are parsed.
func hasArg(name string) bool {
	for _, a := range os.Args[2:] {
		if a == name || a == "-"+strings.TrimPrefix(name, "--") {
			return true
		}
	}
	return false
}

var errMismatch = errors.New("render does not match")

// options is the flag set shared by the commands.
type options struct {
	rpc        string
	out        string
	gasCap     uint64
	band       int
	workers    int
	meter      string
	every      int
	saveStream string
	stream     string
	noiseLib   string
	against    string
	checkpoint string
	noCkpt     bool
	quiet      bool
	noColor    bool

	gateways     multiFlag
	fetchTimeout time.Duration
	noCache      bool

	fork       bool
	forkURL    string
	forkBlock  uint64
	anvil      string
	chunkSteps uint64

	set map[string]bool // flags given explicitly
}

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func parseFlags(name string, args []string, positional int) (*options, []string, error) {
	o := &options{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.rpc, "rpc", "", "")
	fs.StringVar(&o.out, "o", "", "")
	fs.StringVar(&o.out, "out", "", "")
	fs.Uint64Var(&o.gasCap, "gas-cap", 50_000_000, "")
	fs.IntVar(&o.band, "band", 32, "")
	fs.IntVar(&o.workers, "workers", 8, "")
	fs.StringVar(&o.meter, "meter", "auto", "")
	fs.IntVar(&o.every, "measure-every", 10, "")
	fs.StringVar(&o.saveStream, "save-stream", "", "")
	fs.StringVar(&o.stream, "stream", "", "")
	fs.StringVar(&o.noiseLib, "noise-lib", "", "")
	fs.StringVar(&o.against, "against", "", "")
	fs.StringVar(&o.checkpoint, "checkpoint", "", "")
	fs.BoolVar(&o.noCkpt, "no-checkpoint", false, "")
	fs.BoolVar(&o.quiet, "quiet", false, "")
	fs.BoolVar(&o.noColor, "no-color", false, "")
	fs.Var(&o.gateways, "ipfs-gateway", "")
	fs.DurationVar(&o.fetchTimeout, "fetch-timeout", 60*time.Second, "")
	fs.BoolVar(&o.noCache, "no-cache", false, "")
	fs.BoolVar(&o.fork, "fork", false, "")
	fs.StringVar(&o.forkURL, "fork-url", "", "")
	fs.Uint64Var(&o.forkBlock, "fork-block", 0, "")
	fs.StringVar(&o.anvil, "anvil", "", "")
	fs.Uint64Var(&o.chunkSteps, "chunk-steps", 0, "")
	// Allow flags before and after the positional arguments.
	var pos []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, nil, fmt.Errorf("%s: %w\n\n%s", name, err, usage)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	o.set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
	if len(pos) != positional {
		return nil, nil, fmt.Errorf("%s: want %d argument(s), got %d\n\n%s", name, positional, len(pos), usage)
	}
	if o.rpc == "" {
		o.rpc = os.Getenv("OWNABLE_RPC_URL")
	}
	if o.rpc == "" {
		o.rpc = os.Getenv("ETH_RPC_URL")
	}
	if o.forkURL != "" {
		o.fork = true
	}
	if o.rpc == "" && o.forkURL == "" {
		return nil, nil, errors.New("no RPC endpoint: pass --rpc URL or set OWNABLE_RPC_URL")
	}
	if o.gasCap < 1_000_000 {
		return nil, nil, fmt.Errorf("--gas-cap %d is too small to run anything", o.gasCap)
	}
	return o, pos, nil
}

func meterMode(s string) (rpc.GasMeter, error) {
	switch s {
	case "auto":
		return rpc.MeterAuto, nil
	case "trace":
		return rpc.MeterTrace, nil
	case "estimate":
		return rpc.MeterEstimate, nil
	case "off":
		return rpc.MeterOff, nil
	}
	return 0, fmt.Errorf("--meter %q: want auto, trace, estimate or off", s)
}

func parseToken(s string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || v.Sign() < 0 || v.BitLen() > 256 {
		return nil, fmt.Errorf("token id %q: want a non-negative decimal integer", s)
	}
	return v, nil
}

func chainName(id uint64) string {
	switch id {
	case 1:
		return "Ethereum mainnet"
	case 11155111:
		return "Sepolia"
	}
	return "chain " + strconv.FormatUint(id, 10)
}

func cmdInfo(ctx context.Context, args []string) error {
	o, pos, err := parseFlags("info", args, 1)
	if err != nil {
		return err
	}
	url := o.rpc
	if o.forkURL != "" {
		url = o.forkURL
	}
	c := rpc.New(url)
	chain, err := c.ChainID(ctx)
	if err != nil {
		return err
	}
	p, err := piece.Open(ctx, c, pos[0])
	if err != nil {
		return err
	}
	rp, err := p.RenderPath(ctx)
	if err != nil {
		return err
	}
	prog, err := p.Program(ctx)
	if err != nil {
		return err
	}
	params, err := p.Params(ctx)
	if err != nil {
		return err
	}
	name, _ := p.String(ctx, "name")
	symbol, _ := p.String(ctx, "symbol")
	desc, _ := p.String(ctx, "kohiDescription")
	supply, hasSupply := p.TotalSupply(ctx)

	rep := ui.New(os.Stdout, o.quiet, o.noColor)
	ui.New(os.Stderr, o.quiet, o.noColor).Header(version, tagline)
	f := func(label, format string, a ...any) { rep.Field(label, fmt.Sprintf(format, a...), 12) }
	f("piece", "%s (%s)", p.Address, chainName(chain))
	f("IKohiPiece", "yes (ERC-165 0x%x)", abi.IKohiPieceID)
	if name != "" || symbol != "" {
		f("name", "%s (%s)", name, symbol)
	}
	if hasSupply {
		f("minted", "%s (totalSupply)", supply)
	} else {
		f("minted", "unknown (no totalSupply)")
	}
	f("family", "%d, %s", rp.Family, piece.FamilyName(rp.Family))
	f("noise", "%d, %s", rp.NoiseKind, piece.NoiseKindName(rp.NoiseKind))
	f("runner", "%s (one-shot interpreter)", rp.Runner)
	f("stepRunner", "%s (stepped interpreter)", stepLabel(rp.StepRunner))
	f("renderer", "%s", rp.Renderer)
	f("program", "%d bytes, sha256 %s", len(prog), sha(prog))
	if len(prog) >= 10 && string(prog[:4]) == "KVM1" {
		f("canvas", "%dx%d (container header)", int(prog[6])<<8|int(prog[7]), int(prog[8])<<8|int(prog[9]))
	}
	f("params", "%d slot(s)", len(params))
	if desc != "" {
		f("description", "%s", desc)
	}
	if supported(rp.Family) == "" {
		f("render", "%s (ownable render %s <tokenId>)", rep.OK("supported"), p.Address)
	} else {
		f("render", "%s", rep.Warn(supported(rp.Family)))
	}
	rep.Blank()
	return nil
}

func stepLabel(a abi.Address) string {
	if a.IsZero() {
		return "none (the piece fits one call)"
	}
	return a.String()
}

// supported returns "" when this version can render the family, else why not.
func supported(family uint8) string {
	if family == 3 {
		return ""
	}
	return fmt.Sprintf("not yet supported by this version (family %d, %s)", family, piece.FamilyName(family))
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func gasText(st rpc.Stats) string {
	total, exact := st.Estimate()
	if st.Rejected > 0 {
		return "gas not measured (this node's eth_estimateGas stops at 16,777,216 and it has no debug_traceCall)"
	}
	if st.Measured == 0 {
		return "gas not measured"
	}
	if exact {
		return fmt.Sprintf("%s gas (max %s per call, all calls measured via %s)", commas(total), commas(st.MaxGas), st.How)
	}
	return fmt.Sprintf("about %s gas (extrapolated from %d of %d calls measured via %s; max %s per call)",
		commas(total), st.Measured, st.Calls, st.How, commas(st.MaxGas))
}

// gasShort is gasText for the transcript: the total, or that there is none.
func gasShort(st rpc.Stats) string {
	total, exact := st.Estimate()
	switch {
	case st.Rejected > 0 || st.Measured == 0:
		return "gas not measured"
	case exact:
		return commas(total) + " gas"
	}
	return "about " + commas(total) + " gas"
}

// shortStep is stepLabel with the address abbreviated.
func shortStep(a abi.Address) string {
	if a.IsZero() {
		return "none"
	}
	return short(a.String())
}

func commas(v uint64) string {
	s := strconv.FormatUint(v, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func cmdRender(ctx context.Context, args []string, verify bool) error {
	name := "render"
	if verify {
		name = "verify"
	}
	o, pos, err := parseFlags(name, args, 2)
	if err != nil {
		return err
	}
	meter, err := meterMode(o.meter)
	if err != nil {
		return err
	}
	token, err := parseToken(pos[1])
	if err != nil {
		return err
	}
	// tr is the transcript (progress, on stderr); rep is the report (the
	// result lines, on stdout).
	tr := ui.New(os.Stderr, o.quiet, o.noColor)
	defer tr.Close()
	rep := ui.New(os.Stdout, o.quiet, o.noColor)
	tr.Header(version, tagline)
	fetcher := &ref.Fetcher{Gateways: o.gateways, Timeout: o.fetchTimeout, Agent: "ownable/" + version}
	if !o.noCache {
		fetcher.CacheDir = ref.DefaultCacheDir()
	}
	// An explicit reference is read before any chain work, so a bad path or
	// an unreachable host fails at once rather than after the render.
	var refImg *refImage
	if verify && o.against != "" {
		r, err := ref.Parse(o.against)
		if err != nil {
			return fmt.Errorf("--against: %w", err)
		}
		if refImg, err = loadRef(ctx, fetcher, r, tr); err != nil {
			return err
		}
	}
	t0 := time.Now()

	nd, err := openNode(ctx, o, tr)
	if err != nil {
		return err
	}
	defer nd.close()
	c := nd.client
	gasCap := o.gasCap
	if nd.forked {
		gasCap = fork.CallGas
		if !o.set["meter"] {
			meter = rpc.MeterOff // measuring would execute every call twice
		}
	}
	chain, err := c.ChainID(ctx)
	if err != nil {
		return err
	}
	p, err := piece.Open(ctx, c, pos[0])
	if err != nil {
		return err
	}
	rp, err := p.RenderPath(ctx)
	if err != nil {
		return err
	}
	if why := supported(rp.Family); why != "" {
		return errors.New(why)
	}
	seed, err := p.Seed(ctx, token)
	if err != nil {
		return err
	}
	tokenName, err := p.TokenString(ctx, "kohiName", token)
	if err != nil {
		tokenName = fmt.Sprintf("token %s", token)
	}
	tr.Row("piece", joinDot(tokenName, p.Address, chainName(chain)))
	tr.Row("render path", fmt.Sprintf("family %d (%s) · noise %s", rp.Family, piece.FamilyName(rp.Family), piece.NoiseKindName(rp.NoiseKind)))
	tr.Note(fmt.Sprintf("stepRunner %s · renderer %s", shortStep(rp.StepRunner), short(rp.Renderer.String())))
	if nd.forked {
		tr.Row("seed", fmt.Sprintf("%d · executed by %s", seed, nd.short))
	} else {
		tr.Row("seed", fmt.Sprintf("%d · gas cap %s per call", seed, commas(gasCap)))
	}
	if verify && refImg == nil {
		// No --against: the token's own metadata names the image under test.
		if refImg, err = refFromTokenURI(ctx, p, token, max(gasCap, tokenURIGas), fetcher, tr); err != nil {
			return err
		}
	}

	out := o.out
	if out == "" && !verify {
		sym, _ := p.String(ctx, "symbol")
		if sym == "" {
			sym = "piece"
		}
		out = fmt.Sprintf("%s-%s.png", strings.ToLower(sym), token)
	}
	ckpt := o.checkpoint
	if ckpt == "" && out != "" {
		ckpt = out + ".ckpt"
	}
	if o.noCkpt || nd.forked && !o.set["checkpoint"] {
		// A fork render is short, and a checkpoint shared between modes
		// would mix a stream the upstream node computed with one computed
		// locally, so fork mode keeps none unless asked.
		ckpt = ""
	}

	// 1. The draw stream: the program run by the piece's stepped interpreter.
	var streamBytes []byte
	var streamNote string
	streamCaller := rpc.NewCaller(c, gasCap, meter)
	streamCaller.Every = o.every
	tStream := time.Now()
	if o.stream != "" {
		if streamBytes, err = os.ReadFile(o.stream); err != nil {
			return err
		}
		streamNote = "read from " + o.stream + " (NOT produced by the chain in this run)"
		tr.Row("stream", tr.Warn(streamNote))
	} else {
		if rp.StepRunner.IsZero() {
			return errors.New("the piece names no stepped interpreter; one-shot execution is not supported by this version")
		}
		prog, err := p.Program(ctx)
		if err != nil {
			return err
		}
		params, err := p.Params(ctx)
		if err != nil {
			return err
		}
		noiseLib := o.noiseLib
		if noiseLib == "" && rp.NoiseKind == 0 {
			noiseLib = discoverNoiseLib(ctx, c, rp.Renderer, chain)
		}
		tr.Row("program", fmt.Sprintf("%s bytes · sha256 %s", commas(uint64(len(prog))), sha(prog)))

		// How long this run should be, from the runs already finished.
		key := stepkvm.RunKey(rp.StepRunner.String(), prog, seed, params)
		hist := history.Default()
		sv := newStreamView(history.Lookup(hist.Load(), hex.EncodeToString(key[:]), sha(prog)))
		live := tr.Live("stream", sv.view)
		scfg := &stepkvm.Config{
			Caller: streamCaller, StepRunner: rp.StepRunner.String(), NoiseLib: noiseLib,
			Program: prog, Seed: seed, Params: params, Checkpoint: ckpt, KeepDone: true,
			OnResume: func(calls, n int) {
				tr.Note(fmt.Sprintf("resuming from %s: %d calls, %s stream bytes already done", ckpt, calls, commas(uint64(n))))
			},
			OnProgress: func(pr stepkvm.Progress) {
				if pr.Phase == "noise" {
					tr.Phase("noise", fmt.Sprintf("table built in %d calls", pr.Calls), true)
					return
				}
				sv.update(pr)
			},
		}
		if nd.forked {
			// No gas cap binds on a local node: the fewer calls the better.
			steps := o.chunkSteps
			if steps == 0 {
				steps = forkChunkSteps
			}
			scfg.InitialBudget, scfg.MaxBudget = steps, steps
		}
		res, err := stepkvm.Stream(ctx, scfg)
		if err != nil {
			live.Close("stopped after "+clock(time.Since(tStream)), "", false)
			if ckpt != "" && !errors.Is(err, stepkvm.ErrNoCheckpoint) {
				if _, statErr := os.Stat(ckpt); statErr == nil {
					tr.Note(fmt.Sprintf("progress saved to %s; run the same command again to resume", ckpt))
				}
			}
			return err
		}
		streamCaller.Wait()
		streamBytes = res.Stream
		streamNote = fmt.Sprintf("%d executeChunk calls + %d noise-table calls", res.Calls, res.NoiseCalls)
		if res.Retries > 0 {
			streamNote += fmt.Sprintf(" (%d retried smaller)", res.Retries)
		}
		if res.Resumed > 0 {
			streamNote += fmt.Sprintf(", %d of them in an earlier run (checkpoint)", res.Resumed)
		}
		live.Close(joinDot(compact(float64(res.Steps))+" steps", bytesText(len(streamBytes)), clock(time.Since(tStream))),
			streamNote+", "+gasShort(streamCaller.Stats()), true)
		hist.Add(history.Run{
			Key: hex.EncodeToString(key[:]), Program: sha(prog), Steps: res.Steps,
			StreamBytes: len(streamBytes), Calls: res.Calls, When: time.Now(),
		})
	}
	if o.saveStream != "" {
		if err := os.WriteFile(o.saveStream, streamBytes, 0o644); err != nil {
			return err
		}
	}
	dStream := time.Since(tStream)
	s, err := stamp.Parse(streamBytes)
	if err != nil {
		return err
	}
	tr.Row("draw stream", fmt.Sprintf("%s bytes · %s stamps · sha256 %s", commas(uint64(len(streamBytes))), commas(uint64(s.Stamps)), sha(streamBytes)))

	// 2. The pixels: the stream rasterized by the piece's renderer, in bands.
	rasterCaller := rpc.NewCaller(c, gasCap, meter)
	rasterCaller.Every = o.every
	tRaster := time.Now()
	rv := newRasterView()
	rcfg := &stamp.Config{
		Caller: rasterCaller, Renderer: rp.Renderer.String(), BandH: o.band, Workers: o.workers,
		OnProgress: rv.update,
	}
	if nd.forked {
		if !o.set["workers"] {
			rcfg.Workers = forkWorkers()
		}
		if !o.set["band"] {
			rcfg.BandH = forkBand(s.Height, rcfg.Workers)
		}
		rcfg.MaxOps, rcfg.MaxSliceBytes = 1<<30, 1<<30
	}
	live := tr.Live("raster", rv.view)
	rres, err := stamp.Render(ctx, rcfg, s)
	if err != nil {
		live.Close("stopped after "+clock(time.Since(tRaster)), "", false)
		return err
	}
	dRaster := time.Since(tRaster)
	ss, rs := streamCaller.Stats(), rasterCaller.Stats()
	live.Close(joinDot(fmt.Sprintf("%d bands of %d rows", rres.Bands, rcfg.BandH), clock(dRaster)),
		fmt.Sprintf("%d renderTileP calls, %s", rs.Calls, gasShort(rs)), true)
	pixSHA := imgout.SHA256(rres.RGBA)

	if out != "" {
		if err := imgout.WritePNG(out, rres.RGBA, s.Width, s.Height); err != nil {
			return err
		}
	}
	if ckpt != "" {
		os.Remove(ckpt) // the render is complete; the saved stream is no longer needed
	}

	// The report. On a terminal the transcript above already showed the
	// stream and raster phases, so the report leaves them out.
	shown := tr.TTY() && tr.Shows()
	f := func(label, format string, a ...any) { rep.Field(label, fmt.Sprintf(format, a...), 12) }
	tr.Blank()
	rep.Rule()
	f("token", "%s", tokenName)
	image := fmt.Sprintf("%dx%d", s.Width, s.Height)
	if out != "" {
		image += " -> " + out
	}
	f("image", "%s", image)
	if !shown {
		if o.stream != "" {
			f("stream", "%s", streamNote)
		} else {
			f("stream", "%s, %s, %s", streamNote, durText(dStream), gasText(ss))
		}
		f("raster", "%d renderTileP calls, %d bands, %s, %s", rs.Calls, rres.Bands, durText(dRaster), gasText(rs))
	}
	gs, exactS := ss.Estimate()
	gr, exactR := rs.Estimate()
	if o.stream != "" {
		gs, exactS = 0, true // no interpreter calls in this run
	}
	total := "gas not measured"
	measuredS := ss.Measured > 0 && ss.Rejected == 0 || o.stream != ""
	if measuredS && rs.Measured > 0 && rs.Rejected == 0 {
		total = commas(gs+gr) + " gas"
		if !exactS || !exactR {
			total = "about " + total
		}
	}
	if nd.forked {
		f("total", "%d eth_calls to %s; %s; %s", ss.Calls+rs.Calls, nd.short, total, durText(time.Since(t0)))
	} else {
		f("total", "%d eth_calls, each with a %s gas limit; %s; %s", ss.Calls+rs.Calls, commas(gasCap), total, durText(time.Since(t0)))
	}
	if shown && (ss.Rejected > 0 || rs.Rejected > 0) {
		f("", "%s", rep.Label("gas not measured: this node's eth_estimateGas stops at 16,777,216 and it has no debug_traceCall"))
	}
	if n := c.Throttled.Load(); n > 0 {
		f("", "%s", rep.Label(fmt.Sprintf("the endpoint rate-limited %d requests; they were waited out and retried", n)))
	}
	if ss.Misses+rs.Misses > 0 {
		f("", "%s", rep.Label(fmt.Sprintf("%d calls did not fit the cap and were retried smaller", ss.Misses+rs.Misses)))
	}
	rep.Field("stream sha256", sha(streamBytes), 13)
	rep.Field("rgba sha256", pixSHA, 13)
	if nd.forked {
		f("computed by", "%s", rep.Warn(nd.statement))
	} else {
		f("computed by", "%s", nd.statement)
	}

	if !verify {
		rep.Stamp(true, fmt.Sprintf("wrote %s, %s as its contracts compute it", out, tokenName))
		return nil
	}
	refImg.report(rep)
	if refImg.w != s.Width || refImg.h != s.Height {
		f("verify", "%s: the reference is %dx%d, the render is %dx%d", rep.Bad("MISMATCH"), refImg.w, refImg.h, s.Width, s.Height)
		rep.Stamp(false, "the reference is not this token's image")
		return errMismatch
	}
	n, d := imgout.Diff(refImg.rgba, rres.RGBA)
	if n == 0 {
		f("verify", "%s: every pixel of the reference equals the render", rep.OK("IDENTICAL"))
		rep.Stamp(true, fmt.Sprintf("%s is exactly what its contracts compute", tokenName))
		return nil
	}
	f("verify", "%s: %d pixels differ from the reference (max channel delta %d)", rep.Bad("MISMATCH"), n, d)
	rep.Stamp(false, "the reference differs from what the contracts compute")
	return errMismatch
}

func durText(d time.Duration) string {
	return d.Round(time.Second).String()
}

// knownNoiseLib is the NoiseV1Ext deployment per chain, used only when the
// renderer does not say which one it links.
var knownNoiseLib = map[uint64]string{
	1: "0x45bf3Ee5dA57b922D93125991E45F444A6E4B28E",
}

// discoverNoiseLib asks the renderer which NoiseV1Ext it links (noiseLib()),
// falling back to the known deployment for the chain.
func discoverNoiseLib(ctx context.Context, c *rpc.Client, renderer abi.Address, chain uint64) string {
	ret, err := c.EthCall(ctx, renderer.String(), abi.EncodeNoArgs("noiseLib"), 1_000_000, "latest")
	if err == nil {
		if a, err := abi.NewRet(ret).AddressAt(0); err == nil && !a.IsZero() {
			return a.String()
		}
	}
	return knownNoiseLib[chain]
}
