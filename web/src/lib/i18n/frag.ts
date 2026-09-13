// A fragment is one page's messages in both languages. The type makes the
// Russian side carry exactly the keys the English side declares, so a
// missing or misspelled translation fails `make web-check` instead of
// showing up as a raw key in the UI.
export type Fragment<T extends Record<string, string>> = { en: T; ru: { [K in keyof T]: string } };

export function frag<T extends Record<string, string>>(f: Fragment<T>): Fragment<T> {
  return f;
}
