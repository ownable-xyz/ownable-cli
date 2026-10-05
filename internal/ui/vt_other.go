// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

//go:build !windows && !unix

package ui

import "os"

func enableVT(*os.File) bool { return false }

func termWidth(*os.File) int { return 0 }
