"use client";
import { useRouter } from "next/navigation";
import { useTranslations } from "next-intl";
import { useTransition } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { removeUser } from "@/lib/api-client/v1/actions/users";
import { getErrorMessage } from "@/lib/utils/error-messages";

interface DeleteUserConfirmProps {
  user?: {
    id: number;
    name: string;
    keys: { id: number; name: string }[];
  };
}

export function DeleteUserConfirm({
  user,
  onSuccess,
}: DeleteUserConfirmProps & { onSuccess?: () => void }) {
  const router = useRouter();
  const [isPending, startTransition] = useTransition();
  const t = useTranslations("dashboard.deleteUserConfirm");
  const tErrors = useTranslations("errors");

  const handleConfirm = () => {
    if (!user) return;
    startTransition(async () => {
      try {
        const res = await removeUser(user.id);
        if (!res.ok) {
          // REST 桥接返回的 error 是通用 detail，真实原因在 errorCode 中
          const message = res.errorCode
            ? getErrorMessage(tErrors, res.errorCode, res.errorParams)
            : res.error || t("errors.deleteFailed");
          toast.error(message);
          return;
        }
        onSuccess?.();
        router.refresh();
      } catch (error) {
        console.error("删除用户失败:", error);
        toast.error(t("errors.retryError"));
      }
    });
  };

  return (
    <>
      <DialogHeader>
        <DialogTitle>{t("title")}</DialogTitle>
        <DialogDescription>
          {t("question", { name: user?.name ?? "" })}
          <br />
          {t("warning", { count: user?.keys.length || 0 })}
        </DialogDescription>
      </DialogHeader>

      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline" disabled={isPending}>
            {t("cancel")}
          </Button>
        </DialogClose>
        <Button variant="destructive" onClick={handleConfirm} disabled={isPending}>
          {isPending ? t("confirmLoading") : t("confirm")}
        </Button>
      </DialogFooter>
    </>
  );
}
