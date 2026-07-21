import { LinkProps } from '@tanstack/react-router';

interface User {
  name: string;
  email: string;
  avatar: string;
}

interface Team {
  name: string;
  logo: React.ElementType;
  description: string;
}

interface BaseNavItem {
  title: string;
  badge?: string;
  icon?: React.ElementType;
  isDisabled?: boolean;
}

type NavLink = BaseNavItem & {
  url: LinkProps['to'];
  external?: false;
  items?: never;
  mobileOnly?: boolean;
};

type NavExternalLink = BaseNavItem & {
  href: string;
  external: true;
  description?: string;
  items?: never;
  mobileOnly?: boolean;
};

type NavCollapsible = BaseNavItem & {
  items: (BaseNavItem & { url: LinkProps['to'] })[];
  url?: never;
};

type NavItem = NavCollapsible | NavLink | NavExternalLink;

interface NavGroup {
  title: string;
  items: NavItem[];
  bottom?: boolean;
}

interface SidebarData {
  user: User;
  teams: Team[];
  navGroups: NavGroup[];
}

export type { SidebarData, NavGroup, NavItem, NavCollapsible, NavExternalLink, NavLink };
