package zfs_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zrepl/zrepl/zfs"
)

type ResumeTokenTest struct {
	Msg         string
	Token       string
	ExpectToken *zfs.ResumeToken
	ExpectError error
}

func (rtt *ResumeTokenTest) Test(t *testing.T) {
	t.Log(rtt.Msg)
	res, err := zfs.ParseResumeToken(context.TODO(), rtt.Token)

	if rtt.ExpectError != nil {
		assert.EqualValues(t, rtt.ExpectError, err)
		return
	}
	if rtt.ExpectToken != nil {
		assert.Nil(t, err)
		assert.EqualValues(t, rtt.ExpectToken, res)
		return
	}
}

func TestParseResumeToken(t *testing.T) {

	t.SkipNow() // FIXME not compatible with docker

	tbl := []ResumeTokenTest{
		{
			Msg:   "normal send (non-incremental)",
			Token: `1-c41e551c7-c0-789c636064000310a500c4ec50360710e72765a5269740d80cd8e4d3d28a534b18e00024cf86249f5459925a0ca47fec62c2aabf243fbd34338581e1da95456b763eeb58678024cf0996cf4bcc4d65602830d4071a54a2509e5992a1505c90989cea9004b31300818b1bfb`,
			ExpectToken: &zfs.ResumeToken{
				HasToGUID: true,
				ToGUID:    0xae88e6b9aca2d4d6,
				ToName:    "p1/test with space@b",
			},
		},
		{
			Msg:   "normal send (incremental)",
			Token: `1-bf8cd08c7-e8-789c636064000310a501c49c50360710a715e5e7a69766a6304001ffedc71f63d5de782900d9ec48eaf293b252934b206c10c0904f4b2b4e2d618003903c1b927c5265496a31902e5062c2aabf241fe28a6b5716add9f9ac639d01923c27583e2f313715a8df501f68508942796649864271416272aa4312cc4e00275f22e9`,
			ExpectToken: &zfs.ResumeToken{
				HasToGUID:   true,
				ToGUID:      0xae88e6b9aca2d4d6,
				HasFromGUID: true,
				FromGUID:    0x4aec265df1e3db0f,
				ToName:      "p1/test with space@b",
			},
		},
		{
			Msg:         "corrupted token",
			Token:       `1-bf31b879a-b8-789c636064000310a500c4ec50360710e72765a5269740f80cd8e4d3d28a534b18e00024cf86249f5459925acc802a8facbf243fbd3433858161f5ddb9ab1ae7c7466a20c97382e5f312735319180af2f3730cf58166953824c2cc0200cd12345`,
			ExpectError: zfs.ResumeTokenCorruptError,
		},
		{
			Msg: "raw send full (of encrypted dataset)",
			Token: ` 1-10a600a705-110-789c636064000310a500c4ec50360710e72765a5269740d80cd8e4d3d28a534b18e00024cf86249f5459925a0ca46b1498b1ea2fc94f2fcd4c6160b0f16f096c31bd36d503499e132c9f97989bcac05054909f9fa39f919f9baa9f5cec909b98579a98136f64606869606160116f60696861641a1f1ae28cec0e6e0684bf92f3730b8a528b8bf3b32162125077c2e48b12cb61520c00d72d24d5`,
			ExpectToken: &zfs.ResumeToken{
				HasToGUID: true,
				ToGUID: 0x95d6358451844f3c,
				ToName: "rpool/home/cs@manual_20190808_091825_UTC",
				
			},
		}
	}

	for _, test := range tbl {
		test.Test(t)
	}

}
