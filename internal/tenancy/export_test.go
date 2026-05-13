package tenancy

import "time"

// SetClockForTest swaps the cache's clock function. Tests use it to
// deterministically advance past the TTL; production code never sets it.
func SetClockForTest(c *CachingResolver, clock func() time.Time) {
	c.now = clock
}
