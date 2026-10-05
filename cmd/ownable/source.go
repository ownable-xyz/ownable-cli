// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/ownable-xyz/ownable-cli/internal/fork"
	"github.com/ownable-xyz/ownable-cli/internal/imgout"
	"github.com/ownable-xyz/ownable-cli/internal/piece"
	"github.com/ownable-xyz/ownable-cli/internal/ref"
	"github.com/ownable-xyz/ownable-cli/internal/rpc"
	"github.com/ownable-xyz/ownable-cli/internal/ui"
)

// Fork-mode work split, chosen by measurement on Rakel. Against a local node
// the gas cap does not bind, and the time goes into executing the EVM:
//
//   - The interpreter's calls must run one after another, and its speed is
//     the node's EVM speed; the number of calls hardly matters once each one
//     runs millions of steps (the whole program in one call measured slightly
//     slower than 2,000,000-step calls). Calls of a few million steps also
//     keep progress visible.
//   - The renderer's bands are independent, and anvil runs concurrent calls
//     on separate cores, so the canvas is cut into one band per worker. One
//     full-canvas call took 2m45s on token 1; 16 bands on 16 workers, 17s.
const forkChunkSteps = 2_000_000

// forkWorkers is the number of raster bands rendered at once in fork mode.
func forkWorkers() int { return min(max(runtime.NumCPU(), 2), 16) }

// forkBand is the raster band height in fork mode: one band per worker.
func forkBand(height, workers int) int { return (height + workers - 1) / workers }

// tokenURIGas is the least gas tokenURI is read with. Metadata that embeds a
// player page (Rakel's is over 500 KB) needs more than 16,777,216 gas, the
// cap some nodes put on eth_estimateGas, so it is read with eth_call at no
// less than this.
const tokenURIGas = 50_000_000

// node is where the calls go, and what the report says about it.
type node struct {
	client    *rpc.Client
	forked    bool   // a local node: no gas cap, no upstream execution
	short     string // a few words for progress lines
	statement string // the full sentence for the report
	close     func()
}

// openNode connects to the endpoint, or in fork mode starts (or connects to)
// the local node.
func openNode(ctx context.Context, o *options, tr *ui.Term) (*node, error) {
	if o.forkURL != "" {
		block, err := fork.BlockNumber(ctx, o.forkURL)
		if err != nil {
			return nil, fmt.Errorf("--fork-url %s does not answer eth_blockNumber: %w", o.forkURL, err)
		}
		tr.Phase("node", fmt.Sprintf("%s at block %d (a node you started)", o.forkURL, block), true)
		return &node{
			client: rpc.New(o.forkURL), forked: true, short: o.forkURL,
			statement: fmt.Sprintf("your node at %s (block %d). Weaker than a default run: "+
				"the result is only as good as that node.", o.forkURL, block),
			close: func() {},
		}, nil
	}
	if !o.fork {
		return &node{
			client: rpc.New(o.rpc), short: "the RPC endpoint",
			statement: "the RPC endpoint, which ran every call (you trust it to run the EVM correctly, nothing else).",
			close:     func() {},
		}, nil
	}

	anvil, err := fork.FindAnvil(o.anvil)
	if err != nil {
		return nil, err
	}
	up := rpc.New(o.rpc)
	chain, err := up.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("upstream endpoint: %w", err)
	}
	block := o.forkBlock
	if block == 0 {
		if block, err = up.BlockNumber(ctx); err != nil {
			return nil, fmt.Errorf("upstream endpoint: %w", err)
		}
	}
	logf, err := os.CreateTemp("", "ownable-anvil-*.log")
	if err != nil {
		return nil, err
	}
	t := time.Now()
	live := tr.Live("fork", spinView(fmt.Sprintf("starting anvil, forked from the upstream endpoint at block %d", block)))
	n, err := fork.Start(ctx, fork.Options{Anvil: anvil, Upstream: o.rpc, Block: block, Log: logf}, 2*time.Minute)
	if err != nil {
		live.Close("anvil did not start", "", false)
		logf.Close()
		return nil, fmt.Errorf("%w (anvil's output is in %s)", err, logf.Name())
	}
	live.Close(fmt.Sprintf("anvil at %s, block %d, ready in %s", n.URL, block, time.Since(t).Round(100*time.Millisecond)), "", true)
	return &node{
		client: rpc.New(n.URL), forked: true, short: fmt.Sprintf("a local fork at block %d", block),
		statement: fmt.Sprintf("anvil on this machine, forked from %s at block %d. Weaker than a default run: "+
			"the calls ran locally, not on the RPC endpoint.", chainName(chain), block),
		close: func() {
			n.Stop()
			logf.Close()
			os.Remove(logf.Name())
		},
	}, nil
}

// refImage is the image a render is verified against, and where it came from.
type refImage struct {
	res      *ref.Result
	via      string // how the reference was chosen
	metadata string // how the metadata was read, when it came from tokenURI
	rgba     []byte
	w, h     int
}

func (r *refImage) report(rep *ui.Term) {
	f := func(label, format string, a ...any) { rep.Field(label, fmt.Sprintf(format, a...), 12) }
	if r.metadata != "" {
		f("metadata", "%s", r.metadata)
	}
	if r.via != "" {
		f("reference", "%s (%s)", r.res.Source, r.via)
	} else {
		f("reference", "%s", r.res.Source)
	}
	switch {
	case r.res.Cached && r.res.URL != "":
		f("", "from the local cache; first downloaded from %s", r.res.URL)
	case r.res.Cached:
		f("", "from the local cache")
	case r.res.Gateway != "":
		f("", "downloaded via %s", hostOf(r.res.Gateway))
	case r.res.URL != "":
		f("", "downloaded from %s", r.res.URL)
	}
	f("", "%s bytes, sha256 %s, %dx%d", commas(uint64(len(r.res.Data))), r.res.SHA256, r.w, r.h)
}

// loadRef fetches a reference and decodes it as a PNG.
func loadRef(ctx context.Context, f *ref.Fetcher, r ref.Ref, tr *ui.Term) (*refImage, error) {
	live := tr.Live("reference", spinView("fetching "+shortRef(r.String())))
	res, err := f.Fetch(ctx, r)
	if err != nil {
		live.Close("could not fetch "+r.String(), "", false)
		return nil, fmt.Errorf("reference: %w", err)
	}
	rgba, w, h, err := imgout.DecodePNG(res.Data)
	if err != nil {
		live.Close(res.Source+" is not a PNG", "", false)
		return nil, fmt.Errorf("reference %s (%d bytes) is not a PNG this tool can compare: %w", res.Source, len(res.Data), err)
	}
	from := ""
	if res.Cached {
		from = "from the cache"
	}
	live.Close(joinDot(shortRef(res.Source), fmt.Sprintf("%dx%d", w, h), bytesText(len(res.Data)), from), "", true)
	for _, why := range res.Failed {
		tr.Note("tried " + why)
	}
	return &refImage{res: res, rgba: rgba, w: w, h: h}, nil
}

// refFromTokenURI reads the token's metadata and fetches its "image".
func refFromTokenURI(ctx context.Context, p *piece.Piece, token *big.Int, gas uint64, f *ref.Fetcher, tr *ui.Term) (*refImage, error) {
	live := tr.Live("metadata", spinView(fmt.Sprintf("reading tokenURI(%s)", token)))
	uri, err := p.TokenURI(ctx, token, gas)
	if err != nil {
		live.Close(fmt.Sprintf("tokenURI(%s) failed", token), "", false)
		var rev *rpc.RevertError
		if errors.As(err, &rev) {
			return nil, fmt.Errorf("%w; pass --against with the image to compare", err)
		}
		return nil, err
	}
	meta, mref, err := ref.MetadataJSON(uri)
	if err != nil {
		live.Close(fmt.Sprintf("tokenURI(%s) is not metadata this tool reads", token), "", false)
		return nil, err
	}
	form := metadataForm(uri)
	desc := fmt.Sprintf("tokenURI(%s): %s, %s bytes", token, form, commas(uint64(len(uri))))
	if mref != nil {
		live.Close(fmt.Sprintf("tokenURI(%s) points to %s", token, short(mref.String())), "", true)
		live = tr.Live("metadata", spinView("fetching "+shortRef(mref.String())))
		res, err := f.Fetch(ctx, *mref)
		if err != nil {
			live.Close("could not fetch the metadata", "", false)
			return nil, fmt.Errorf("metadata: %w", err)
		}
		meta = res.Data
		desc = fmt.Sprintf("tokenURI(%s): %s, %s bytes of JSON, sha256 %s", token, mref, commas(uint64(len(meta))), res.SHA256)
	}
	size := bytesText(len(uri)) // the same measure as the report: the tokenURI as read
	if mref != nil {
		size = bytesText(len(meta)) + " of JSON"
	}
	live.Close(joinDot(fmt.Sprintf("tokenURI(%s)", token), metadataForm(uri), size), "", true)
	image, err := ref.ImageField(meta)
	if err != nil {
		return nil, err
	}
	r, err := ref.Parse(image)
	if err != nil {
		return nil, fmt.Errorf("metadata image: %w", err)
	}
	if r.Kind == ref.Local {
		return nil, fmt.Errorf("metadata image %q is not a URL, an IPFS path or a data: URI", image)
	}
	img, err := loadRef(ctx, f, r, tr)
	if err != nil {
		return nil, err
	}
	img.via = fmt.Sprintf("the \"image\" of tokenURI(%s)", token)
	img.metadata = desc
	return img, nil
}

// metadataForm names the form of a tokenURI for the report.
func metadataForm(uri string) string {
	if len(uri) >= 5 && strings.EqualFold(uri[:5], "data:") {
		head, _, _ := strings.Cut(uri, ",")
		return head
	}
	if i := strings.Index(uri, "://"); i > 0 {
		return uri[:i] + " URL"
	}
	return "URI"
}
