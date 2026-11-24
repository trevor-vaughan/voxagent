/* LD_PRELOAD interposer recording every libaugeas call ruby-augeas makes, for
 * the augeas design (spec §10). Built and used by trace.sh. One line per call:
 * <handle> <function> <args...> => <result>. Handles are numbered h1, h2, ...
 * in aug_init order (a closed handle's number is never reused), so traces
 * compare across runs. h0 is a handle not opened through aug_init here. Output: $AUGTRACE. */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct augeas augeas;
static FILE *out;
static const void *hs[256];
static int nh;
/* libaugeas calls its own exported functions through the PLT, so calls made
 * while one is running are internal and are not logged. */
static __thread int depth;
static FILE *devnull;

static FILE *o(void) {
  if (depth) { if (!devnull) devnull = fopen("/dev/null", "w"); return devnull; }
  if (!out) { const char *p = getenv("AUGTRACE"); out = fopen(p ? p : "/tmp/augtrace.log", "a"); setvbuf(out, NULL, _IOLBF, 0); }
  return out;
}
static int hid(const void *a) { for (int i = 0; i < nh; i++) if (hs[i] == a) return i + 1; return 0; }
static void q(const char *s) {
  if (!s) { fputs(" NULL", o()); return; }
  fputs(" \"", o());
  for (; *s; s++) { if (*s == '\n') fputs("\\n", o()); else if (*s == '"' || *s == '\\') fprintf(o(), "\\%c", *s); else fputc(*s, o()); }
  fputc('"', o());
}
#define REAL(name) static __typeof__(&name) real_##name; if (!real_##name) real_##name = (__typeof__(&name))dlsym(RTLD_NEXT, #name)

augeas *aug_init(const char *root, const char *loadpath, unsigned int flags) {
  REAL(aug_init);
  depth++; augeas *a = (real_aug_init(root, loadpath, flags)); depth--;
  if (a && nh < 256) hs[nh++] = a;
  fprintf(o(), "h%d aug_init", hid(a)); q(root); q(loadpath); fprintf(o(), " %u\n", flags);
  return a;
}
void aug_close(augeas *a) {
  REAL(aug_close);
  int i = hid(a);
  fprintf(o(), "h%d aug_close\n", i);
  depth++; (real_aug_close(a)); depth--;
  if (i) hs[i - 1] = NULL; /* a later handle at the same address gets a new number */
}
int aug_get(const augeas *a, const char *p, const char **v) {
  REAL(aug_get);
  depth++; int r = (real_aug_get(a, p, v)); depth--;
  fprintf(o(), "h%d aug_get", hid(a)); q(p); fprintf(o(), " => %d", r); if (v) q(*v); fputc('\n', o());
  return r;
}
int aug_set(augeas *a, const char *p, const char *v) {
  REAL(aug_set);
  depth++; int r = (real_aug_set(a, p, v)); depth--;
  fprintf(o(), "h%d aug_set", hid(a)); q(p); q(v); fprintf(o(), " => %d\n", r);
  return r;
}
int aug_rm(augeas *a, const char *p) {
  REAL(aug_rm);
  depth++; int r = (real_aug_rm(a, p)); depth--;
  fprintf(o(), "h%d aug_rm", hid(a)); q(p); fprintf(o(), " => %d\n", r);
  return r;
}
int aug_insert(augeas *a, const char *p, const char *l, int before) {
  REAL(aug_insert);
  depth++; int r = (real_aug_insert(a, p, l, before)); depth--;
  fprintf(o(), "h%d aug_insert", hid(a)); q(p); q(l); fprintf(o(), " %d => %d\n", before, r);
  return r;
}
int aug_match(const augeas *a, const char *p, char ***m) {
  REAL(aug_match);
  depth++; int r = (real_aug_match(a, p, m)); depth--;
  fprintf(o(), "h%d aug_match", hid(a)); q(p); fprintf(o(), " => %d", r);
  if (m && r > 0) for (int i = 0; i < r; i++) q((*m)[i]);
  fputc('\n', o());
  return r;
}
int aug_load(augeas *a) {
  REAL(aug_load);
  depth++; int r = (real_aug_load(a)); depth--;
  fprintf(o(), "h%d aug_load => %d\n", hid(a), r);
  return r;
}
int aug_save(augeas *a) {
  REAL(aug_save);
  depth++; int r = (real_aug_save(a)); depth--;
  fprintf(o(), "h%d aug_save => %d\n", hid(a), r);
  return r;
}
int aug_defvar(augeas *a, const char *n, const char *e) {
  REAL(aug_defvar);
  depth++; int r = (real_aug_defvar(a, n, e)); depth--;
  fprintf(o(), "h%d aug_defvar", hid(a)); q(n); q(e); fprintf(o(), " => %d\n", r);
  return r;
}
int aug_defnode(augeas *a, const char *n, const char *e, const char *v, int *c) {
  REAL(aug_defnode);
  depth++; int r = (real_aug_defnode(a, n, e, v, c)); depth--;
  fprintf(o(), "h%d aug_defnode", hid(a)); q(n); q(e); q(v); fprintf(o(), " %s => %d\n", c ? "created" : "NULL", r);
  return r;
}
int aug_text_store(augeas *a, const char *l, const char *n, const char *p) {
  REAL(aug_text_store);
  depth++; int r = (real_aug_text_store(a, l, n, p)); depth--;
  fprintf(o(), "h%d aug_text_store", hid(a)); q(l); q(n); q(p); fprintf(o(), " => %d\n", r);
  return r;
}
/* Calls these types should never make; logged so a trace shows if they do. */
#define UNEXPECTED(name) int name(void *a) { REAL(name); fprintf(o(), "h%d " #name " UNEXPECTED\n", hid(a)); return real_##name(a); }
UNEXPECTED(aug_error)
const char *aug_error_message(void *a) { REAL(aug_error_message); fprintf(o(), "h%d aug_error_message UNEXPECTED\n", hid(a)); return real_aug_error_message(a); }
const char *aug_error_details(void *a) { REAL(aug_error_details); fprintf(o(), "h%d aug_error_details UNEXPECTED\n", hid(a)); return real_aug_error_details(a); }
const char *aug_error_minor_message(void *a) { REAL(aug_error_minor_message); fprintf(o(), "h%d aug_error_minor_message UNEXPECTED\n", hid(a)); return real_aug_error_minor_message(a); }
int aug_label(void *a, const char *p, const char **l) { REAL(aug_label); fprintf(o(), "h%d aug_label UNEXPECTED", hid(a)); q(p); fputc('\n', o()); return real_aug_label(a, p, l); }
int aug_mv(augeas *a, const char *s, const char *d) {
  REAL(aug_mv);
  depth++; int r = (real_aug_mv(a, s, d)); depth--;
  fprintf(o(), "h%d aug_mv", hid(a)); q(s); q(d); fprintf(o(), " => %d\n", r);
  return r;
}
