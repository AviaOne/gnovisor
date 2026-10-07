// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package version holds GnoVisor's version.
package version

// Version is the published version. A release build overwrites it from the
// exact tag on HEAD (Makefile, -ldflags -X); any other build keeps this
// default.
var Version = "1.0.0"

// Attribution is the notice every copy and every modified version must keep
// (NOTICE.md, additional term under section 7 b of the AGPL-3.0).
const Attribution = "GnoVisor, by AviaOne.com"
