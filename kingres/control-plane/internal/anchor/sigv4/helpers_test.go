// SPDX-License-Identifier: AGPL-3.0-only

package sigv4

import "github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"

func anchorSecret(s string) anchor.Secret { return anchor.Secret(s) }
