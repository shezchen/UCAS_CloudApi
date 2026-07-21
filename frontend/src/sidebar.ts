import {
  IconLayoutDashboard,
  IconPackages,
  IconSettings,
  IconUsers,
  IconRobot,
  IconShield,
  IconKey,
  IconActivity,
  IconDatabase,
  IconAB2,
  IconBaselineDensityMedium,
  IconAi,
  IconNote,
  IconChartBar,
  IconBooks,
  IconHeartHandshake,
} from '@tabler/icons-react';
import { Command } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { useAuthStore } from '@/stores/authStore';
import { useRoutePermissions } from '@/hooks/useRoutePermissions';
import { useMe } from '@/features/auth/data/auth';
import { type SidebarData, type NavGroup, type NavLink } from './components/layout/types';

export function useSidebarData(): SidebarData {
  const { t } = useTranslation();
  const { user: authUser } = useAuthStore((state) => state.auth);
  const { data: meData } = useMe();
  const { filterNavGroups } = useRoutePermissions();

  // Use data from me query if available, otherwise fall back to auth store
  const user = meData || authUser;

  // Generate user initials for avatar
  const getInitials = (firstName?: string, lastName?: string, email?: string) => {
    if (firstName && lastName) {
      return `${firstName.charAt(0)}${lastName.charAt(0)}`.toUpperCase();
    }
    if (firstName) {
      return firstName.slice(0, 2).toUpperCase();
    }
    if (email) {
      return email.split('@')[0].slice(0, 2).toUpperCase();
    }
    return 'U';
  };

  // Generate user display name
  const getDisplayName = (firstName?: string, lastName?: string, email?: string) => {
    if (firstName && lastName) {
      return `${firstName} ${lastName}`;
    }
    if (firstName) {
      return firstName;
    }
    if (email) {
      const username = email.split('@')[0];
      return username.charAt(0).toUpperCase() + username.slice(1);
    }
    return 'User';
  };

  // Keep the full management navigation for the system owner.
  const ownerNavGroups: NavGroup[] = [
    {
      title: t('sidebar.groups.admin'),
      items: [
        {
          title: t('sidebar.items.dashboard'),
          url: '/',
          icon: IconLayoutDashboard,
        } as NavLink,
        {
          title: t('sidebar.items.projects'),
          url: '/projects',
          icon: IconPackages,
        } as NavLink,
        {
          title: t('sidebar.items.channels'),
          url: '/channels',
          icon: IconAi,
        } as NavLink,
        {
          title: t('sidebar.items.models'),
          url: '/models',
          icon: IconRobot,
        } as NavLink,
        {
          title: t('sidebar.items.promptProtectionRules'),
          url: '/prompt-protection-rules',
          icon: IconShield,
        } as NavLink,
        {
          title: t('sidebar.items.dataStorages'),
          url: '/data-storages',
          icon: IconDatabase,
        } as NavLink,
        {
          title: t('sidebar.items.users'),
          url: '/users',
          icon: IconUsers,
        } as NavLink,
        {
          title: t('sidebar.items.roles'),
          url: '/roles',
          icon: IconShield,
        } as NavLink,
        // {
        //   title: 'Permission Demo',
        //   url: '/permission-demo',
        //   icon: IconSettings,
        // } as NavLink,
      ],
    },
    {
      title: t('sidebar.groups.project'),
      items: [
        {
          title: t('sidebar.items.resources'),
          url: '/project/resources',
          icon: IconBooks,
        } as NavLink,
        {
          title: t('sidebar.items.apiKeys'),
          url: '/project/api-keys',
          icon: IconKey,
        } as NavLink,
        {
          title: t('sidebar.items.prompts'),
          url: '/project/prompts',
          icon: IconNote,
        } as NavLink,
        {
          title: t('sidebar.items.requests'),
          url: '/project/requests',
          icon: IconActivity,
        } as NavLink,
        {
          title: t('sidebar.items.usageStats'),
          url: '/project/usage-stats',
          icon: IconChartBar,
        } as NavLink,
        // {
        //   title: t('sidebar.items.usageLogs'),
        //   url: '/project/usage-logs',
        //   icon: IconActivityHeartbeat,
        // } as NavLink,
        {
          title: t('sidebar.items.traces'),
          url: '/project/traces',
          icon: IconAB2,
        } as NavLink,
        {
          title: t('sidebar.items.threads'),
          url: '/project/threads',
          icon: IconBaselineDensityMedium,
        } as NavLink,

        {
          title: t('sidebar.items.users'),
          url: '/project/users',
          icon: IconUsers,
        } as NavLink,
        {
          title: t('sidebar.items.roles'),
          url: '/project/roles',
          icon: IconShield,
        } as NavLink,
        {
          title: t('sidebar.items.playground'),
          url: '/project/playground',
          icon: IconRobot,
        } as NavLink,
      ],
    },
    {
      title: t('sidebar.groups.settings'),
      items: [
        {
          title: t('sidebar.items.system'),
          url: '/system',
          icon: IconSettings,
          mobileOnly: true,
        } as NavLink,
        // {
        //   title: 'Account',
        //   url: '/settings/account',
        //   icon: IconTool,
        // } as NavLink,
        // {
        //   title: 'Appearance',
        //   url: '/settings/appearance',
        //   icon: IconPalette,
        // } as NavLink,
        // {
        //   title: 'Notifications',
        //   url: '/settings/notifications',
        //   icon: IconNotification,
        // } as NavLink,
      ],
    },
  ];

  // Campus members primarily consume the shared API. Donating an upstream
  // channel remains available, but it is deliberately a separate, secondary
  // action rather than the default interpretation of “Channels”.
  const memberNavGroups: NavGroup[] = [
    {
      title: t('sidebar.groups.campusSharing'),
      items: [
        {
          title: t('sidebar.items.startUsing'),
          url: '/project/resources',
          icon: IconBooks,
        } as NavLink,
        {
          title: t('sidebar.items.myApiKeys'),
          url: '/project/api-keys',
          icon: IconKey,
        } as NavLink,
        {
          title: t('sidebar.items.usageLeaderboard'),
          url: '/project/usage-stats',
          icon: IconChartBar,
        } as NavLink,
        {
          title: t('sidebar.items.donateChannel'),
          url: '/channels',
          icon: IconHeartHandshake,
        } as NavLink,
      ],
    },
    {
      title: t('sidebar.groups.settings'),
      items: [
        {
          title: t('sidebar.items.system'),
          url: '/system',
          icon: IconSettings,
          mobileOnly: true,
        } as NavLink,
      ],
    },
  ];

  const rawNavGroups = user?.isOwner ? ownerNavGroups : memberNavGroups;

  // 使用权限过滤导航组
  const filteredNavGroups = filterNavGroups(rawNavGroups);

  return {
    user: {
      name: getDisplayName(user?.firstName, user?.lastName, user?.email),
      email: user?.email || 'user@example.com',
      avatar: user?.avatar || getInitials(user?.firstName, user?.lastName, user?.email),
    },
    teams: [
      {
        name: t('sidebar.team.name'),
        logo: Command,
        description: '',
        // DO NOT USE THIS
        // plan: t('sidebar.team.plan'),
      },
    ],
    navGroups: filteredNavGroups,
  };
}
