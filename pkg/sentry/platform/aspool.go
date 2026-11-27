// gvisor-windows: a pool of ready address spaces.

package platform

import "sync"

// poolPlatform hands out address spaces that were created in advance.
type poolPlatform struct {
	Platform
	spares int

	mu      sync.Mutex
	ready   []AddressSpace
	pending int // being created
}

// NewASPool returns p with up to spares address spaces created ahead of
// time. Creating an address space is slow on a platform that starts a host
// process for it (about 10 ms for the Windows stub, and fork and execve need
// one each); the pool takes that off the path of fork and execve and refills
// itself in the background. Spaces made with non-default options are not
// pooled. spares <= 0 returns p.
func NewASPool(p Platform, spares int) Platform {
	if spares <= 0 {
		return p
	}
	pp := &poolPlatform{Platform: p, spares: spares}
	pp.mu.Lock()
	pp.refillLocked()
	pp.mu.Unlock()
	return pp
}

// NewAddressSpace implements Platform.NewAddressSpace.
func (p *poolPlatform) NewAddressSpace(opts AddressSpaceOptions) (AddressSpace, error) {
	if opts != (AddressSpaceOptions{}) {
		return p.Platform.NewAddressSpace(opts)
	}
	p.mu.Lock()
	var as AddressSpace
	if n := len(p.ready); n > 0 {
		as = p.ready[n-1]
		p.ready = p.ready[:n-1]
	}
	p.refillLocked()
	p.mu.Unlock()
	if as != nil {
		return as, nil
	}
	return p.Platform.NewAddressSpace(opts)
}

// refillLocked starts the creation of the missing spares.
func (p *poolPlatform) refillLocked() {
	for len(p.ready)+p.pending < p.spares {
		p.pending++
		go func() {
			as, err := p.Platform.NewAddressSpace(AddressSpaceOptions{})
			p.mu.Lock()
			p.pending--
			if err == nil {
				p.ready = append(p.ready, as)
			}
			p.mu.Unlock()
		}()
	}
}
