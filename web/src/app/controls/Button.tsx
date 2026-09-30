import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from "react";
import { Link, type LinkProps } from "react-router-dom";

export type ButtonVariant = "primary" | "secondary" | "danger" | "ghost" | "icon" | "unstyled";
export type ButtonSize = "sm" | "md" | "lg";

type ButtonStyleProps = {
  variant?: ButtonVariant;
  size?: ButtonSize;
  className?: string;
  children?: ReactNode;
};

export type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & ButtonStyleProps;

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button({
  variant = "primary",
  size = "md",
  className = "",
  type = "button",
  children,
  ...props
}, ref) {
  return (
    <button
      {...props}
      ref={ref}
      type={type}
      className={`ui-button ui-button--${variant} ui-button--${size} ${className}`.trim()}
    >
      {children}
    </button>
  );
});

export type ButtonLinkProps = Omit<LinkProps, "className"> & ButtonStyleProps & { className?: string };

export function ButtonLink({
  variant = "secondary",
  size = "md",
  className = "",
  children,
  ...props
}: ButtonLinkProps) {
  return (
    <Link
      {...props}
      className={`ui-button ui-button--${variant} ui-button--${size} ${className}`.trim()}
    >
      {children}
    </Link>
  );
}
