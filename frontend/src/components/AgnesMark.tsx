type Props = {
  size?: number
  className?: string
}

// Agnes "A" monogram: a rounded triangle stem on a plain baseline, drawn to
// the same 24px grid as the other provider marks.
export function AgnesMark({ size = 16, className = '' }: Props) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      className={`block shrink-0 text-foreground ${className}`.trim()}
      aria-hidden="true"
    >
      <path
        fill="currentColor"
        d="M12 3.2 3.6 20.8h3.34l1.86-4.02h6.4l1.86 4.02h3.34L12 3.2Zm-1.85 10.9L12 9.94l1.85 4.16h-3.7Z"
      />
    </svg>
  )
}
