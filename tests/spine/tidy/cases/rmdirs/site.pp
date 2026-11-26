# rmdirs: matched directories go, each after its matched children; d3 goes
# with its unmatched stay (force), and keep stays.
tidy { '/srv/rd': recurse => true, rmdirs => true, matches => ['d*', 'f*'] }
