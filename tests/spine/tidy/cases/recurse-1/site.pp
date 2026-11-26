# SIMP's shape: one level, by basename glob. sub/deep.log is out of reach.
tidy { '/srv/r1': recurse => 1, matches => ['*.log'] }
