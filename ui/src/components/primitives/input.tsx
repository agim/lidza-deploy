import * as React from "react"
import { cn } from "cn"

// Input types the autocomplete attribute does not apply to.
const noAutocomplete = new Set(["checkbox", "radio", "hidden", "file", "submit", "button", "reset", "image"])

// Fields default to autocomplete="off": the console edits identifiers, hosts,
// paths and credentials, which browsers must not autofill or remember. Pass an
// explicit value (email, url, name, one-time-code, new-password…) where the
// browser should help. The default also keeps Chrome from flagging every text
// field next to a password field as a missing-autocomplete problem.
function Input({ className, type, autoComplete, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      autoComplete={autoComplete ?? (type && noAutocomplete.has(type) ? undefined : "off")}
      data-slot="input"
      className={cn(
        "h-8 w-full min-w-0 rounded-lg border border-input bg-transparent px-2.5 py-1 text-base transition-colors outline-none file:inline-flex file:h-6 file:border-0 file:bg-transparent file:text-sm file:font-medium file:text-foreground placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:pointer-events-none disabled:cursor-not-allowed disabled:bg-input/50 disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-3 aria-invalid:ring-destructive/20 md:text-sm dark:bg-input/30 dark:disabled:bg-input/80 dark:aria-invalid:border-destructive/50 dark:aria-invalid:ring-destructive/40",
        className
      )}
      {...props}
    />
  )
}

export { Input }
