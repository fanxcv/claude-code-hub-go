"use client";

import { useTranslations } from "next-intl";
import { useEffect, useState } from "react";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { searchUsersForFilter } from "@/lib/api-client/v1/actions/users";

/** 「全部用户」的哨兵值：Radix Select 不允许空字符串作为 item value（与面板内密钥选择同惯式）。 */
const ALL_USERS_VALUE = "__all__";

interface QuickUserFilterProps {
  userId?: number;
  onUserChange: (userId: number | undefined) => void;
}

/**
 * 折叠态筛选栏上的快捷用户下拉：切换即生效（调用方负责改 URL query），
 * 面板展开后由调用方不渲染，避免与面板内的用户筛选重复。
 */
export function QuickUserFilter({ userId, onUserChange }: QuickUserFilterProps) {
  const t = useTranslations("dashboard");
  const [users, setUsers] = useState<Array<{ id: number; name: string }>>([]);

  useEffect(() => {
    let isMounted = true;

    const loadUsers = async () => {
      try {
        const result = await searchUsersForFilter();
        if (!isMounted) return;
        if (result.ok) {
          setUsers(result.data);
        } else {
          console.error("Failed to load users for quick filter:", result.error);
          setUsers([]);
        }
      } catch (error) {
        if (!isMounted) return;
        console.error("Failed to load users for quick filter:", error);
        setUsers([]);
      }
    };

    void loadUsers();
    return () => {
      isMounted = false;
    };
  }, []);

  const selectedUserName = users.find((user) => user.id === userId)?.name;

  return (
    <Select
      value={userId?.toString() ?? ALL_USERS_VALUE}
      onValueChange={(value) =>
        onUserChange(value === ALL_USERS_VALUE ? undefined : Number.parseInt(value, 10))
      }
    >
      <SelectTrigger
        size="sm"
        aria-label={t("logs.filters.quickUser")}
        className="max-w-[180px] text-muted-foreground"
      >
        <SelectValue>
          {userId === undefined
            ? t("logs.filters.allUsers")
            : (selectedUserName ?? userId.toString())}
        </SelectValue>
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={ALL_USERS_VALUE}>{t("logs.filters.allUsers")}</SelectItem>
        {users.map((user) => (
          <SelectItem key={user.id} value={user.id.toString()}>
            {user.name}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
