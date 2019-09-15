package tests

import (
	"fmt"

	"github.com/zrepl/zrepl/platformtest"
	"github.com/zrepl/zrepl/zfs"
)

func RollupRelease(ctx *platformtest.Context) {

	platformtest.Run(ctx, platformtest.PanicErr, ctx.RootDataset, `
		DESTROYROOT
		CREATEROOT
		+  "foo bar"
		+  "foo bar@1"
		+  "foo bar@2"
		+  "foo bar@3"
		+  "foo bar@4"
		+  "foo bar@5"
		+  "foo bar@6"
		R  zfs hold zrepl_platformtest   "${ROOTDS}/foo bar@1"
		R  zfs hold zrepl_platformtest_2 "${ROOTDS}/foo bar@2"
		R  zfs hold zrepl_platformtest   "${ROOTDS}/foo bar@3"
		R  zfs hold zrepl_platformtest   "${ROOTDS}/foo bar@5"
		R  zfs hold zrepl_platformtest   "${ROOTDS}/foo bar@6"
	`)

	ds := fmt.Sprintf("%s/foo bar", ctx.RootDataset)
	tag := "zrepl_platformtest"
	guid4, err := zfs.ZFSGetGUID(ds, "@5")
	if err != nil {
		panic(err)
	}

	err = zfs.ZFSReleaseAllOlderAndIncludingGUID(ctx, ds, guid4, tag)
	if err != nil {
		panic(err)
	}

	expTags := []struct {
		Snap  string
		Holds map[string]bool
	}{
		{"1", map[string]bool{}},
		{"2", map[string]bool{"zrepl_platformtest_2": true}},
		{"3", map[string]bool{}},
		{"4", map[string]bool{}},
		{"5", map[string]bool{}},
		{"6", map[string]bool{"zrepl_platformtest": true}},
	}

	for _, exp := range expTags {
		holds, err := zfs.ZFSHolds(ctx, ds, exp.Snap)
		if err != nil {
			panic(err)
		}
		for _, h := range holds {
			if e, ok := exp.Holds[h]; !ok || !e {
				panic(fmt.Sprintf("tag %q on snap %q not expected", h, exp.Snap))
			}
		}
	}

	platformtest.Run(ctx, platformtest.PanicErr, ctx.RootDataset, `
	R zfs release zrepl_platformtest_2 "${ROOTDS}/foo bar@2"
	R zfs release zrepl_platformtest "${ROOTDS}/foo bar@6"
	-  "foo bar@1"
	-  "foo bar@2"
	-  "foo bar@3"
	-  "foo bar@4"
	-  "foo bar@5"
	-  "foo bar@6"
	-  "foo bar"
	`)

}
