(* deliberately does not compile: apply must never read $libdir/augeas/lenses *)
module Modprobe =
  let lns = this_is_unbound
