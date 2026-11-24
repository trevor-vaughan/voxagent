node default {
  notify { 'hello': message => 'from the harness fixture' }
}
