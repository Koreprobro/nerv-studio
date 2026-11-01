package gpu

import "errors"

// listROCm is the ROCm (AMD) enumerator stub (audit M5 / D4).
//
// Planned contract (mirrors the CUDA path): parse `rocminfo` / `rocm-smi
// --showproductname` into physical GPU rows, re-index them by
// HIP_VISIBLE_DEVICES, and have the scheduler export HIP_VISIBLE_DEVICES
// (physical ids) per dispatched job instead of CUDA_VISIBLE_DEVICES. Deferred
// until AMD hardware is available; until then this returns an explicit
// not-implemented error so the gap stays visible instead of silently treating
// AMD GPUs as absent.
func listROCm() ([]GPU, error) {
	return nil, errors.New("rocm: enumeration not implemented (rocminfo backend deferred)")
}
