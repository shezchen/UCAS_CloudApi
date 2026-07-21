import { ReactNode } from 'react';
import { Link, useLocation } from '@tanstack/react-router';
import { ChevronRight, ExternalLink } from 'lucide-react';
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible';
import {
  SidebarGroup,
  SidebarGroupLabel,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarMenuSub,
  SidebarMenuSubButton,
  SidebarMenuSubItem,
  useSidebar,
} from '@/components/ui/sidebar';
import { Badge } from '../ui/badge';
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '../ui/dropdown-menu';
import { NavCollapsible, NavExternalLink, NavItem, NavLink, type NavGroup } from './types';

export function NavGroup({ title, items }: NavGroup) {
  const { state, isMobile } = useSidebar();
  const href = useLocation({ select: (location) => location.href });

  const visibleItems = items.filter((item) => {
    if (!item.items) {
      if (item.isDisabled) return false;
      if ((item as NavLink).mobileOnly && !isMobile) return false;
      return true;
    }

    const hasVisibleSubItems = item.items.some((subItem) => !subItem.isDisabled);
    return hasVisibleSubItems && !item.isDisabled;
  });

  // 如果没有可见的菜单项，不渲染整个分组
  if (visibleItems.length === 0) {
    return null;
  }

  return (
    <SidebarGroup>
      <SidebarGroupLabel>{title}</SidebarGroupLabel>
      <SidebarMenu>
        {visibleItems.map((item) => {
          const key = `${item.title}-${'href' in item ? item.href : item.url}`;

          if (!item.items) return <SidebarMenuLink key={key} item={item} href={href} />;

          if (state === 'collapsed' && !isMobile) return <SidebarMenuCollapsedDropdown key={key} item={item} href={href} />;

          return <SidebarMenuCollapsible key={key} item={item} href={href} />;
        })}
      </SidebarMenu>
    </SidebarGroup>
  );
}

const NavBadge = ({ children }: { children: ReactNode }) => <Badge className='rounded-full px-1 py-0 text-xs'>{children}</Badge>;

const SidebarMenuLink = ({ item, href }: { item: NavLink | NavExternalLink; href: string }) => {
  const { setOpenMobile } = useSidebar();
  const tooltip =
    item.external && item.description
      ? {
          children: (
            <div className='space-y-1'>
              <div>{item.title}</div>
              <div className='text-muted-foreground text-xs'>{item.description}</div>
            </div>
          ),
        }
      : item.title;

  return (
    <SidebarMenuItem>
      <SidebarMenuButton asChild isActive={checkIsActive(href, item)} tooltip={tooltip} className='h-12 rounded-2xl transition-all'>
        {item.external ? (
          <a href={item.href} target='_blank' rel='noopener noreferrer' onClick={() => setOpenMobile(false)}>
            {item.icon && <item.icon className='text-xl' />}
            <span className='flex min-w-0 flex-1 flex-col'>
              <span className='truncate'>{item.title}</span>
              {item.description && (
                <span className='text-muted-foreground truncate text-xs group-data-[collapsible=icon]:hidden'>{item.description}</span>
              )}
            </span>
            {item.badge && <NavBadge>{item.badge}</NavBadge>}
            <ExternalLink className='ml-auto h-3.5 w-3.5 opacity-70' aria-hidden='true' />
          </a>
        ) : (
          <Link to={item.url} onClick={() => setOpenMobile(false)}>
            {item.icon && <item.icon className='text-xl' />}
            <span>{item.title}</span>
            {item.badge && <NavBadge>{item.badge}</NavBadge>}
          </Link>
        )}
      </SidebarMenuButton>
    </SidebarMenuItem>
  );
};

const SidebarMenuCollapsible = ({ item, href }: { item: NavCollapsible; href: string }) => {
  const { setOpenMobile } = useSidebar();
  // 只渲染未被禁用的子项
  const visibleSubItems = item.items.filter((subItem) => !subItem.isDisabled);

  return (
    <Collapsible asChild defaultOpen={checkIsActive(href, item, true)} className='group/collapsible'>
      <SidebarMenuItem>
        <CollapsibleTrigger asChild>
          <SidebarMenuButton tooltip={item.title} className='h-12 rounded-2xl transition-all'>
            {item.icon && <item.icon className='text-xl' />}
            <span>{item.title}</span>
            {item.badge && <NavBadge>{item.badge}</NavBadge>}
            <ChevronRight className='ml-auto transition-transform duration-200 group-data-[state=open]/collapsible:rotate-90' />
          </SidebarMenuButton>
        </CollapsibleTrigger>
        <CollapsibleContent className='CollapsibleContent'>
          <SidebarMenuSub>
            {visibleSubItems.map((subItem) => (
              <SidebarMenuSubItem key={subItem.title}>
                <SidebarMenuSubButton asChild isActive={checkIsActive(href, subItem)}>
                  <Link to={subItem.url} onClick={() => setOpenMobile(false)}>
                    {subItem.icon && <subItem.icon />}
                    <span>{subItem.title}</span>
                    {subItem.badge && <NavBadge>{subItem.badge}</NavBadge>}
                  </Link>
                </SidebarMenuSubButton>
              </SidebarMenuSubItem>
            ))}
          </SidebarMenuSub>
        </CollapsibleContent>
      </SidebarMenuItem>
    </Collapsible>
  );
};

const SidebarMenuCollapsedDropdown = ({ item, href }: { item: NavCollapsible; href: string }) => {
  // 只渲染未被禁用的子项
  const visibleSubItems = item.items.filter((subItem) => !subItem.isDisabled);

  return (
    <SidebarMenuItem>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <SidebarMenuButton tooltip={item.title} isActive={checkIsActive(href, item)} className='h-12 rounded-2xl transition-all'>
            {item.icon && <item.icon className='text-xl' />}
            <span>{item.title}</span>
            {item.badge && <NavBadge>{item.badge}</NavBadge>}
            <ChevronRight className='ml-auto transition-transform duration-200 group-data-[state=open]/collapsible:rotate-90' />
          </SidebarMenuButton>
        </DropdownMenuTrigger>
        <DropdownMenuContent side='right' align='start' sideOffset={4}>
          <DropdownMenuLabel>
            {item.title} {item.badge ? `(${item.badge})` : ''}
          </DropdownMenuLabel>
          <DropdownMenuSeparator />
          {visibleSubItems.map((sub) => (
            <DropdownMenuItem key={`${sub.title}-${sub.url}`} asChild>
              <Link to={sub.url} className={`${checkIsActive(href, sub) ? 'bg-secondary' : ''}`}>
                {sub.icon && <sub.icon />}
                <span className='max-w-52 text-wrap'>{sub.title}</span>
                {sub.badge && <span className='ml-auto text-xs'>{sub.badge}</span>}
              </Link>
            </DropdownMenuItem>
          ))}
        </DropdownMenuContent>
      </DropdownMenu>
    </SidebarMenuItem>
  );
};

function checkIsActive(href: string, item: NavItem, mainNav = false) {
  if ('external' in item && item.external) {
    return false;
  }

  return (
    href === item.url || // /endpint?search=param
    href.split('?')[0] === item.url || // endpoint
    !!item?.items?.filter((i) => i.url === href).length || // if child nav is active
    (mainNav && href.split('/')[1] !== '' && href.split('/')[1] === item?.url?.split('/')[1])
  );
}
